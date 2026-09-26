package selfupdate

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestUpdater returns an Updater that reports a fixed platform and
// executable path, so planning can be tested for every platform from any host.
func newTestUpdater(goos, goarch, exe, appImage string) *Updater {
	return &Updater{
		config: Config{
			AppImageBaseName: "EVE_Buddy",
			AssetBaseName:    "evebuddy",
			BundleName:       "EVE Buddy.app",
			ExeName:          "EVE Buddy.exe",
			TarBinaryPath:    "usr/local/bin/evebuddy",
		},
		executablePath: func() (string, error) { return exe, nil },
		goarch:         goarch,
		goos:           goos,
		appImagePath:   func() string { return appImage },
		// Tests legitimately run out of a temporary directory, so the transient
		// location guard is pointed at a path nothing can be under.
		tempDir: func() string { return filepath.Join(string(filepath.Separator), "no-such-temp-dir") },
	}
}

func TestAssetName(t *testing.T) {
	cases := []struct {
		name    string
		goos    string
		goarch  string
		kind    Kind
		want    string
		wantErr bool
	}{
		{"windows amd64", "windows", "amd64", KindWindowsExe, "evebuddy-1.2.3-windows-x64.zip", false},
		{"linux amd64", "linux", "amd64", KindLinuxTar, "evebuddy-1.2.3-linux-amd64.tar.xz", false},
		{"appimage amd64", "linux", "amd64", KindAppImage, "EVE_Buddy-1.2.3-x86_64.AppImage", false},
		{"darwin arm64", "darwin", "arm64", KindMacBundle, "evebuddy-1.2.3-darwin-arm64.zip", false},
		{"darwin amd64", "darwin", "amd64", KindMacBundle, "evebuddy-1.2.3-darwin-intel64.zip", false},
		{"windows arm64 has no asset", "windows", "arm64", KindWindowsExe, "", true},
		{"linux arm64 has no asset", "linux", "arm64", KindLinuxTar, "", true},
		{"unsupported kind", "windows", "amd64", KindUnsupported, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u := newTestUpdater(tc.goos, tc.goarch, "/app/evebuddy", "")
			got, err := u.assetName(tc.kind, "1.2.3")
			if tc.wantErr {
				assert.Error(t, err)
				return
			}
			if assert.NoError(t, err) {
				assert.Equal(t, tc.want, got)
			}
		})
	}
}

// TestAssetNamesMatchRelease guards against the asset naming drifting away from
// what CI actually publishes. The expected names are copied from the asset list
// of a real release.
func TestAssetNamesMatchRelease(t *testing.T) {
	const version = "0.75.0"
	published := []string{
		"evebuddy-0.75.0-darwin-arm64.zip",
		"evebuddy-0.75.0-darwin-intel64.zip",
		"evebuddy-0.75.0-linux-amd64.tar.xz",
		"evebuddy-0.75.0-windows-x64.zip",
		"EVE_Buddy-0.75.0-x86_64.AppImage",
	}
	platforms := []struct {
		goos, goarch string
		kind         Kind
	}{
		{"darwin", "arm64", KindMacBundle},
		{"darwin", "amd64", KindMacBundle},
		{"linux", "amd64", KindLinuxTar},
		{"windows", "amd64", KindWindowsExe},
		{"linux", "amd64", KindAppImage},
	}
	for i, p := range platforms {
		u := newTestUpdater(p.goos, p.goarch, "/app/evebuddy", "")
		got, err := u.assetName(p.kind, version)
		if assert.NoError(t, err) {
			assert.Equal(t, published[i], got, "asset name for %s/%s", p.goos, p.goarch)
		}
	}
}

func TestDetectAppImage(t *testing.T) {
	// Inside a running AppImage os.Executable points into the temporary mount,
	// so the APPIMAGE variable must win.
	const appImage = "/home/chris/Apps/EVE_Buddy.AppImage"
	u := newTestUpdater("linux", "amd64", "/tmp/.mount_abc/usr/bin/evebuddy", appImage)
	kind, target, err := u.detect()
	if assert.NoError(t, err) {
		assert.Equal(t, KindAppImage, kind)
		// The path is made absolute, which on Windows also adds a drive letter.
		want, absErr := filepath.Abs(appImage)
		require.NoError(t, absErr)
		assert.Equal(t, want, target)
	}
}

func TestDetectAppImageIgnoredOnOtherOS(t *testing.T) {
	// A stray APPIMAGE variable must not turn a Windows install into an AppImage.
	dir := t.TempDir()
	exe := filepath.Join(dir, "EVE Buddy.exe")
	require.NoError(t, os.WriteFile(exe, []byte("x"), 0o755))
	u := newTestUpdater("windows", "amd64", exe, "/some/leftover.AppImage")
	kind, target, err := u.detect()
	if assert.NoError(t, err) {
		assert.Equal(t, KindWindowsExe, kind)
		assert.Equal(t, exe, target)
	}
}

func TestDetectUnsupported(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "evebuddy")
	require.NoError(t, os.WriteFile(exe, []byte("x"), 0o755))
	u := newTestUpdater("android", "arm64", exe, "")
	_, _, err := u.detect()
	assert.ErrorIs(t, err, ErrUnsupported)
}

func TestDetectMacBundle(t *testing.T) {
	dir := t.TempDir()
	bundle := filepath.Join(dir, "EVE Buddy.app")
	inner := filepath.Join(bundle, "Contents", "MacOS")
	require.NoError(t, os.MkdirAll(inner, 0o755))
	exe := filepath.Join(inner, "evebuddy")
	require.NoError(t, os.WriteFile(exe, []byte("x"), 0o755))

	u := newTestUpdater("darwin", "arm64", exe, "")
	kind, target, err := u.detect()
	if assert.NoError(t, err) {
		assert.Equal(t, KindMacBundle, kind)
		assert.Equal(t, bundle, target, "the whole bundle must be replaced, not the inner binary")
	}
}

func TestDetectMacBareBinaryIsUnsupported(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "evebuddy")
	require.NoError(t, os.WriteFile(exe, []byte("x"), 0o755))
	u := newTestUpdater("darwin", "arm64", exe, "")
	_, _, err := u.detect()
	assert.ErrorIs(t, err, ErrUnsupported)
}

func TestPlanRejectsUnwritableDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod does not restrict directory writes on Windows")
	}
	dir := t.TempDir()
	appDir := filepath.Join(dir, "app")
	require.NoError(t, os.Mkdir(appDir, 0o755))
	exe := filepath.Join(appDir, "evebuddy")
	require.NoError(t, os.WriteFile(exe, []byte("x"), 0o755))
	require.NoError(t, os.Chmod(appDir, 0o500)) // read + execute only
	t.Cleanup(func() {
		os.Chmod(appDir, 0o755) // so t.TempDir cleanup can remove it
	})

	u := newTestUpdater(runtime.GOOS, "amd64", exe, "")
	_, err := u.Plan("1.2.3")
	assert.ErrorIs(t, err, ErrNotWritable)
}

func TestPlanSucceeds(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "EVE Buddy.exe")
	require.NoError(t, os.WriteFile(exe, []byte("x"), 0o755))

	u := newTestUpdater("windows", "amd64", exe, "")
	p, err := u.Plan("1.2.3")
	if assert.NoError(t, err) {
		assert.Equal(t, KindWindowsExe, p.Kind)
		assert.Equal(t, "evebuddy-1.2.3-windows-x64.zip", p.AssetName)
		assert.Equal(t, exe, p.TargetPath)
		assert.Equal(t, "1.2.3", p.Version)
	}
	// Planning must leave no trace, in particular no leftover write-check file.
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Len(t, entries, 1, "Plan must not leave files behind")
}

func TestCleanupRemovesLeftovers(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "EVE Buddy.exe")
	require.NoError(t, os.WriteFile(exe, []byte("x"), 0o755))
	// A backup left behind because Windows could not delete the running exe.
	require.NoError(t, os.WriteFile(exe+oldSuffix, []byte("old"), 0o755))
	// A staging dir left behind by an interrupted update.
	staging := filepath.Join(dir, stagingPrefix+"123")
	require.NoError(t, os.Mkdir(staging, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(staging, "partial.zip"), []byte("junk"), 0o644))
	// An unrelated file, which must survive.
	keep := filepath.Join(dir, "evebuddy.log")
	require.NoError(t, os.WriteFile(keep, []byte("log"), 0o644))

	u := newTestUpdater(runtime.GOOS, "amd64", exe, "")
	u.Cleanup()

	assert.NoFileExists(t, exe+oldSuffix)
	assert.NoDirExists(t, staging)
	assert.FileExists(t, keep)
	assert.FileExists(t, exe, "the installed app must never be touched by cleanup")
}

func TestSafeJoin(t *testing.T) {
	base := filepath.FromSlash("/dest")
	cases := []struct {
		name    string
		rel     string
		wantErr bool
	}{
		{"plain file", "app.exe", false},
		{"nested", filepath.FromSlash("Contents/MacOS/app"), false},
		{"parent escape", filepath.FromSlash("../evil"), true},
		{"nested parent escape", filepath.FromSlash("a/../../evil"), true},
		{"absolute-ish sibling", filepath.FromSlash("../dest2/x"), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := safeJoin(base, tc.rel)
			if tc.wantErr {
				assert.ErrorIs(t, err, ErrMalformedArchive)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestUnderRoot(t *testing.T) {
	root := "EVE Buddy.app"
	cases := []struct {
		name     string
		in       string
		wantRest string
		wantOK   bool
	}{
		{"root itself", "EVE Buddy.app", "", true},
		{"child", filepath.Join("EVE Buddy.app", "Contents"), "Contents", true},
		{"deep child", filepath.Join("EVE Buddy.app", "Contents", "Info.plist"), filepath.Join("Contents", "Info.plist"), true},
		{"other dir", filepath.Join("__MACOSX", "x"), "", false},
		{"prefix but not child", "EVE Buddy.app.bak", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rest, ok := underRoot(tc.in, root)
			assert.Equal(t, tc.wantOK, ok)
			if tc.wantOK {
				assert.Equal(t, tc.wantRest, rest)
			}
		})
	}
}

func TestSanitizeFileName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"app.zip", "app.zip"},
		{filepath.FromSlash("../../etc/passwd"), "passwd"},
		{"..", "download"},
		{"", "download"},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, sanitizeFileName(tc.in), "input %q", tc.in)
	}
}

func TestVerifyReplacement(t *testing.T) {
	dir := t.TempDir()
	big := filepath.Join(dir, "big.exe")
	require.NoError(t, os.WriteFile(big, make([]byte, 2<<20), 0o755))
	small := filepath.Join(dir, "small.exe")
	require.NoError(t, os.WriteFile(small, []byte("too small"), 0o755))
	bundle := filepath.Join(dir, "App.app")
	require.NoError(t, os.Mkdir(bundle, 0o755))

	assert.NoError(t, verifyReplacement(big, KindWindowsExe))
	assert.Error(t, verifyReplacement(small, KindWindowsExe), "a truncated download must be rejected")
	assert.Error(t, verifyReplacement(bundle, KindWindowsExe), "a directory is not an executable")
	assert.Error(t, verifyReplacement(filepath.Join(dir, "missing.exe"), KindWindowsExe))
	assert.NoError(t, verifyReplacement(bundle, KindMacBundle))
	assert.Error(t, verifyReplacement(big, KindMacBundle), "a file is not an app bundle")
}

func TestPlanRejectsTransientLocation(t *testing.T) {
	// Starting the .exe straight from the downloaded zip in Explorer runs it
	// from a temporary directory. An update there is silently thrown away, so it
	// must be refused before anything is downloaded.
	temp := t.TempDir()
	extracted := filepath.Join(temp, "evebuddy-0.74.0-windows-x64.zip.671")
	require.NoError(t, os.Mkdir(extracted, 0o755))
	exe := filepath.Join(extracted, "EVE Buddy.exe")
	require.NoError(t, os.WriteFile(exe, []byte("x"), 0o755))

	u := newTestUpdater("windows", "amd64", exe, "")
	u.tempDir = func() string { return temp }

	_, err := u.Plan("0.75.0")
	assert.ErrorIs(t, err, ErrTransientLocation)
}

func TestPlanAllowsNormalLocationOutsideTemp(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "EVE Buddy.exe")
	require.NoError(t, os.WriteFile(exe, []byte("x"), 0o755))

	u := newTestUpdater("windows", "amd64", exe, "")
	// A temp dir elsewhere must not disqualify this install.
	u.tempDir = func() string { return filepath.Join(dir, "some-other-temp") }

	_, err := u.Plan("0.75.0")
	assert.NoError(t, err)
}

func TestIsUnder(t *testing.T) {
	base := filepath.FromSlash("/tmp/outer")
	cases := []struct {
		name string
		path string
		want bool
	}{
		{"direct child", filepath.FromSlash("/tmp/outer/app.exe"), true},
		{"nested child", filepath.FromSlash("/tmp/outer/a/b/app.exe"), true},
		{"the dir itself", filepath.FromSlash("/tmp/outer"), true},
		{"sibling", filepath.FromSlash("/tmp/other/app.exe"), false},
		{"parent", filepath.FromSlash("/tmp"), false},
		{"prefix but not child", filepath.FromSlash("/tmp/outerspace/app.exe"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, isUnder(tc.path, base))
		})
	}
	assert.False(t, isUnder(filepath.FromSlash("/tmp/outer/x"), ""), "an empty dir must never match")
}

func TestIsUnderIgnoresCaseOnWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("case folding only applies on Windows")
	}
	assert.True(t, isUnder(`C:\Users\Chris\AppData\Local\Temp\x\app.exe`, `C:\Users\chris\appdata\local\temp`))
}
