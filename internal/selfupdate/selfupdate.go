// Package selfupdate replaces the running application with a newer release.
//
// The update is applied in three distinct phases, so that a failure in an
// early phase can never leave a broken installation behind:
//
//  1. [Updater.Plan] decides whether this build can update itself at all and
//     which release asset it needs. It also verifies up front that the
//     installation directory is writable, so a 30 MB download is not wasted on
//     an app installed into a read-only location.
//  2. [Updater.Download] streams the asset into a staging directory next to the
//     installed app and verifies its checksum.
//  3. [Updater.Apply] unpacks the asset and swaps it in. Only this phase
//     modifies the installation, and it does so with renames only.
//
// Applying an update does not restart the app. The caller is responsible for
// relaunching, which must happen after the app has fully shut down. See
// [Relaunch].
package selfupdate

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Errors returned by this package.
var (
	// ErrUnsupported means this build can not update itself, e.g. on Android.
	ErrUnsupported = errors.New("self-update not supported for this installation")
	// ErrNotWritable means the installation directory can not be modified,
	// e.g. an app installed into Program Files without elevation.
	ErrNotWritable = errors.New("installation directory is not writable")
	// ErrChecksumMismatch means the download did not match the expected digest.
	ErrChecksumMismatch = errors.New("checksum mismatch")
	// ErrAssetMissing means the release has no asset for this platform.
	ErrAssetMissing = errors.New("no matching release asset")
	// ErrTransientLocation means the app is running from a temporary directory,
	// which happens when the .exe is started straight out of the downloaded zip
	// in Explorer instead of being unpacked first. Updating there appears to
	// work but is thrown away with the temporary directory.
	ErrTransientLocation = errors.New("application is running from a temporary directory")
)

// stagingPrefix marks temporary directories created next to the installed app.
// Leftovers are removed by [Cleanup].
const stagingPrefix = ".update-staging-"

// oldSuffix is appended to the replaced app, which on Windows can not be
// deleted while it is still running. Leftovers are removed by [Cleanup].
const oldSuffix = ".old"

// Kind describes how an installation is packaged, which determines how it is
// replaced.
type Kind uint

const (
	KindUnsupported Kind = iota
	// KindWindowsExe is a bare .exe, distributed in a zip.
	KindWindowsExe
	// KindAppImage is a single self-contained executable file.
	KindAppImage
	// KindLinuxTar is a binary distributed inside a tar.xz.
	KindLinuxTar
	// KindMacBundle is a .app directory, distributed in a zip.
	KindMacBundle
)

func (k Kind) String() string {
	switch k {
	case KindWindowsExe:
		return "windows-exe"
	case KindAppImage:
		return "appimage"
	case KindLinuxTar:
		return "linux-tar"
	case KindMacBundle:
		return "mac-bundle"
	}
	return "unsupported"
}

// Config describes how an app's releases are named and packaged.
// The zero value is not usable.
type Config struct {
	// AppImageBaseName is the leading part of an AppImage asset,
	// e.g. "EVE_Buddy" for "EVE_Buddy-1.2.3-x86_64.AppImage".
	AppImageBaseName string
	// AssetBaseName is the leading part of an archive asset,
	// e.g. "evebuddy" for "evebuddy-1.2.3-windows-x64.zip".
	AssetBaseName string
	// BundleName is the name of the macOS app bundle, e.g. "EVE Buddy.app".
	BundleName string
	// ExeName is the name of the executable inside a Windows zip,
	// e.g. "EVE Buddy.exe".
	ExeName string
	// TarBinaryPath is the path of the executable inside a Linux tar.xz,
	// e.g. "usr/local/bin/evebuddy".
	TarBinaryPath string
}

// Plan describes an update that is ready to be downloaded and applied.
type Plan struct {
	// AssetName is the release asset required for this installation.
	AssetName string
	// Kind is how this installation is packaged.
	Kind Kind
	// TargetPath is the file or directory that will be replaced.
	TargetPath string
	// Version is the version being updated to, normalized.
	Version string
}

// Updater applies updates to the running installation.
type Updater struct {
	config Config

	// executablePath returns the path of the running executable.
	// Overridable for tests.
	executablePath func() (string, error)
	// goos and goarch identify the target platform. Overridable for tests.
	goarch string
	goos   string
	// appImagePath is the value of the APPIMAGE environment variable.
	// Overridable for tests.
	appImagePath func() string
	// tempDir returns the system temporary directory. Overridable for tests,
	// which legitimately run out of a temporary directory themselves.
	tempDir func() string
}

// New returns a new Updater for the running installation.
func New(c Config) *Updater {
	return &Updater{
		config:         c,
		executablePath: os.Executable,
		goarch:         runtime.GOARCH,
		goos:           runtime.GOOS,
		appImagePath:   func() string { return os.Getenv("APPIMAGE") },
		tempDir:        os.TempDir,
	}
}

// Plan reports how the running installation would be updated to the given
// version. It returns [ErrUnsupported] when this build can not update itself
// and [ErrNotWritable] when the installation can not be modified.
//
// Plan touches the filesystem but never modifies the installation.
func (u *Updater) Plan(version string) (Plan, error) {
	kind, target, err := u.detect()
	if err != nil {
		return Plan{}, err
	}
	// Refuse before downloading anything: an update applied in a temporary
	// directory would be lost, leaving the user convinced they had updated.
	if isUnder(target, u.tempDir()) {
		return Plan{}, fmt.Errorf("%w: %s", ErrTransientLocation, target)
	}
	asset, err := u.assetName(kind, version)
	if err != nil {
		return Plan{}, err
	}
	// The swap replaces TargetPath by renaming within its parent, so the parent
	// is what needs to be writable, not the target itself.
	if err := checkWritable(filepath.Dir(target)); err != nil {
		return Plan{}, err
	}
	return Plan{
		AssetName:  asset,
		Kind:       kind,
		TargetPath: target,
		Version:    version,
	}, nil
}

// detect reports how the running installation is packaged and what needs to be
// replaced to update it.
func (u *Updater) detect() (Kind, string, error) {
	// An AppImage is checked first, because inside a running AppImage
	// os.Executable reports a path inside the temporary mount point, not the
	// AppImage file itself.
	if p := u.appImagePath(); p != "" && u.goos == "linux" {
		abs, err := filepath.Abs(p)
		if err != nil {
			return KindUnsupported, "", fmt.Errorf("resolve APPIMAGE path: %w", err)
		}
		return KindAppImage, abs, nil
	}
	exe, err := u.executablePath()
	if err != nil {
		return KindUnsupported, "", fmt.Errorf("locate executable: %w", err)
	}
	// Resolve symlinks so that the real file is replaced rather than a link to
	// it, which would otherwise be silently clobbered.
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return KindUnsupported, "", fmt.Errorf("resolve executable path: %w", err)
	}
	switch u.goos {
	case "windows":
		return KindWindowsExe, exe, nil
	case "linux":
		return KindLinuxTar, exe, nil
	case "darwin":
		// Replace the whole .app bundle rather than the binary buried inside
		// it, so that resources and Info.plist stay consistent with the binary.
		if bundle, ok := findBundleRoot(exe); ok {
			return KindMacBundle, bundle, nil
		}
		// A bare binary built with "go build" rather than a packaged bundle.
		return KindUnsupported, "", fmt.Errorf("%w: not running from an app bundle", ErrUnsupported)
	}
	return KindUnsupported, "", fmt.Errorf("%w: %s", ErrUnsupported, u.goos)
}

// assetName returns the release asset needed to update this installation.
func (u *Updater) assetName(k Kind, version string) (string, error) {
	switch k {
	case KindWindowsExe:
		if u.goarch != "amd64" {
			return "", fmt.Errorf("%w: windows %s", ErrAssetMissing, u.goarch)
		}
		return fmt.Sprintf("%s-%s-windows-x64.zip", u.config.AssetBaseName, version), nil
	case KindAppImage:
		if u.goarch != "amd64" {
			return "", fmt.Errorf("%w: appimage %s", ErrAssetMissing, u.goarch)
		}
		return fmt.Sprintf("%s-%s-x86_64.AppImage", u.config.AppImageBaseName, version), nil
	case KindLinuxTar:
		if u.goarch != "amd64" {
			return "", fmt.Errorf("%w: linux %s", ErrAssetMissing, u.goarch)
		}
		return fmt.Sprintf("%s-%s-linux-amd64.tar.xz", u.config.AssetBaseName, version), nil
	case KindMacBundle:
		switch u.goarch {
		case "arm64":
			return fmt.Sprintf("%s-%s-darwin-arm64.zip", u.config.AssetBaseName, version), nil
		case "amd64":
			return fmt.Sprintf("%s-%s-darwin-intel64.zip", u.config.AssetBaseName, version), nil
		}
		return "", fmt.Errorf("%w: darwin %s", ErrAssetMissing, u.goarch)
	}
	return "", ErrUnsupported
}

// findBundleRoot returns the .app bundle containing a binary, walking up from
// the executable, and reports whether one was found.
func findBundleRoot(exe string) (string, bool) {
	dir := exe
	for {
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
		if strings.HasSuffix(dir, ".app") {
			return dir, true
		}
	}
}

// isUnder reports whether path lies inside dir. Both are cleaned first, and the
// comparison is case insensitive on Windows, where paths are.
func isUnder(path, dir string) bool {
	if dir == "" {
		return false
	}
	if runtime.GOOS == "windows" {
		// filepath.Rel compares path segments byte for byte, so fold the case
		// first; only the volume name is folded for us.
		path = strings.ToLower(path)
		dir = strings.ToLower(dir)
	}
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		// Different volumes, so path can not be inside dir.
		return false
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	return !filepath.IsAbs(rel)
}

// checkWritable reports whether files can be created in dir, by creating and
// removing a temporary file. Checking permission bits is not sufficient,
// because on Windows they do not reflect the effective ACL.
func checkWritable(dir string) error {
	f, err := os.CreateTemp(dir, ".write-check-*")
	if err != nil {
		return fmt.Errorf("%w: %s: %w", ErrNotWritable, dir, err)
	}
	name := f.Name()
	f.Close()
	if err := os.Remove(name); err != nil {
		slog.Warn("selfupdate: failed to remove write check file", "path", name, "error", err)
	}
	return nil
}

// Cleanup removes artifacts left behind by an earlier update: the replaced app,
// which on Windows can not be deleted while it is still running, and any
// staging directories abandoned by an interrupted update.
//
// It should be called once during startup. Errors are logged, not returned,
// since leftovers are harmless and must never block startup.
func (u *Updater) Cleanup() {
	_, target, err := u.detect()
	if err != nil {
		return
	}
	if err := os.RemoveAll(target + oldSuffix); err != nil {
		slog.Debug("selfupdate: could not remove previous version", "error", err)
	}
	dir := filepath.Dir(target)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), stagingPrefix) {
			continue
		}
		p := filepath.Join(dir, e.Name())
		if err := os.RemoveAll(p); err != nil {
			slog.Debug("selfupdate: could not remove staging dir", "path", p, "error", err)
		} else {
			slog.Info("selfupdate: removed abandoned staging dir", "path", p)
		}
	}
}

// ensureContext returns ctx's error, so long running loops can abort promptly.
func ensureContext(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}
