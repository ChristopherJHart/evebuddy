package selfupdate

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/jarcoal/httpmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/ulikunitz/xz"

	"github.com/ErikKalkoken/evebuddy/internal/github"
)

// newVersionPayload returns bytes standing in for a new executable. It is over
// the 1 MiB plausibility floor that verifyReplacement enforces.
func newVersionPayload() []byte {
	b := make([]byte, 2<<20)
	copy(b, "NEW VERSION")
	return b
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// makeZip builds a zip archive in memory from a map of archive path to content.
func makeZip(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for name, content := range files {
		f, err := w.Create(name)
		require.NoError(t, err)
		_, err = f.Write(content)
		require.NoError(t, err)
	}
	require.NoError(t, w.Close())
	return buf.Bytes()
}

// makeTarXZ builds a tar.xz archive in memory.
func makeTarXZ(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	xw, err := xz.NewWriter(&buf)
	require.NoError(t, err)
	tw := tar.NewWriter(xw)
	for name, content := range files {
		require.NoError(t, tw.WriteHeader(&tar.Header{
			Name:     name,
			Mode:     0o755,
			Size:     int64(len(content)),
			Typeflag: tar.TypeReg,
		}))
		_, err := tw.Write(content)
		require.NoError(t, err)
	}
	require.NoError(t, tw.Close())
	require.NoError(t, xw.Close())
	return buf.Bytes()
}

// installDir creates a fake installation and returns its directory and the
// path of the installed app.
func installDir(t *testing.T, name string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	target := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(target, []byte("OLD VERSION"), 0o755))
	return dir, target
}

// serveAsset registers a download responder and returns the matching Asset.
func serveAsset(name string, body []byte) github.Asset {
	url := "https://github.com/ErikKalkoken/evebuddy/releases/download/v1.2.3/" + name
	httpmock.RegisterResponder("GET", url, func(*http.Request) (*http.Response, error) {
		resp := httpmock.NewBytesResponse(200, body)
		resp.ContentLength = int64(len(body))
		return resp, nil
	})
	return github.Asset{
		DownloadURL: url,
		Name:        name,
		SHA256:      sha256Hex(body),
		Size:        int64(len(body)),
	}
}

func TestDownloadAndApplyWindows(t *testing.T) {
	httpmock.Activate()
	defer httpmock.DeactivateAndReset()

	dir, target := installDir(t, "EVE Buddy.exe")
	payload := newVersionPayload()
	archive := makeZip(t, map[string][]byte{"EVE Buddy.exe": payload})
	asset := serveAsset("evebuddy-1.2.3-windows-x64.zip", archive)

	u := newTestUpdater("windows", "amd64", target, "")
	plan, err := u.Plan("1.2.3")
	require.NoError(t, err)

	var lastDone, lastTotal int64
	staged, err := u.Download(t.Context(), plan, asset, func(done, total int64) {
		lastDone, lastTotal = done, total
	})
	require.NoError(t, err)
	assert.Equal(t, int64(len(archive)), lastDone, "progress must reach the full size")
	assert.Equal(t, int64(len(archive)), lastTotal)
	assert.DirExists(t, staged.Dir)
	assert.Equal(t, dir, filepath.Dir(staged.Dir), "staging must sit next to the app so the swap is a rename")

	require.NoError(t, u.Apply(t.Context(), staged))

	got, err := os.ReadFile(target)
	require.NoError(t, err)
	assert.Equal(t, payload, got, "the installed app must be the new version")

	// On Windows the old exe is kept for the next start; elsewhere it goes now.
	if runtime.GOOS == "windows" {
		old, err := os.ReadFile(target + oldSuffix)
		if assert.NoError(t, err) {
			assert.Equal(t, []byte("OLD VERSION"), old)
		}
	} else {
		assert.NoFileExists(t, target+oldSuffix)
	}
	assert.NoDirExists(t, staged.Dir, "staging must be cleaned up after a successful apply")
}

func TestDownloadAndApplyAppImage(t *testing.T) {
	httpmock.Activate()
	defer httpmock.DeactivateAndReset()

	_, target := installDir(t, "EVE_Buddy.AppImage")
	payload := newVersionPayload()
	// An AppImage asset is the executable itself, not an archive.
	asset := serveAsset("EVE_Buddy-1.2.3-x86_64.AppImage", payload)

	u := newTestUpdater("linux", "amd64", "/tmp/.mount_x/usr/bin/evebuddy", target)
	plan, err := u.Plan("1.2.3")
	require.NoError(t, err)
	assert.Equal(t, KindAppImage, plan.Kind)

	staged, err := u.Download(t.Context(), plan, asset, nil)
	require.NoError(t, err)
	require.NoError(t, u.Apply(t.Context(), staged))

	got, err := os.ReadFile(target)
	require.NoError(t, err)
	assert.Equal(t, payload, got)

	if runtime.GOOS != "windows" {
		fi, err := os.Stat(target)
		if assert.NoError(t, err) {
			assert.NotZero(t, fi.Mode().Perm()&0o100, "an AppImage must stay executable")
		}
	}
}

func TestDownloadAndApplyLinuxTar(t *testing.T) {
	httpmock.Activate()
	defer httpmock.DeactivateAndReset()

	_, target := installDir(t, "evebuddy")
	payload := newVersionPayload()
	archive := makeTarXZ(t, map[string][]byte{
		// Real releases wrap everything in a top-level directory.
		"evebuddy/usr/local/bin/evebuddy":                              payload,
		"evebuddy/usr/local/share/applications/io.github.test.desktop": []byte("[Desktop Entry]"),
	})
	asset := serveAsset("evebuddy-1.2.3-linux-amd64.tar.xz", archive)

	u := newTestUpdater("linux", "amd64", target, "")
	plan, err := u.Plan("1.2.3")
	require.NoError(t, err)
	assert.Equal(t, KindLinuxTar, plan.Kind)

	staged, err := u.Download(t.Context(), plan, asset, nil)
	require.NoError(t, err)
	require.NoError(t, u.Apply(t.Context(), staged))

	got, err := os.ReadFile(target)
	require.NoError(t, err)
	assert.Equal(t, payload, got)
}

func TestDownloadAndApplyMacBundle(t *testing.T) {
	httpmock.Activate()
	defer httpmock.DeactivateAndReset()

	dir := t.TempDir()
	bundle := filepath.Join(dir, "EVE Buddy.app")
	require.NoError(t, os.MkdirAll(filepath.Join(bundle, "Contents", "MacOS"), 0o755))
	exe := filepath.Join(bundle, "Contents", "MacOS", "evebuddy")
	require.NoError(t, os.WriteFile(exe, []byte("OLD VERSION"), 0o755))

	payload := newVersionPayload()
	archive := makeZip(t, map[string][]byte{
		"EVE Buddy.app/Contents/Info.plist":     []byte("<plist/>"),
		"EVE Buddy.app/Contents/MacOS/evebuddy": payload,
		// Metadata some zip tools add, which must be ignored.
		"__MACOSX/._EVE Buddy.app": []byte("junk"),
	})
	asset := serveAsset("evebuddy-1.2.3-darwin-arm64.zip", archive)

	u := newTestUpdater("darwin", "arm64", exe, "")
	plan, err := u.Plan("1.2.3")
	require.NoError(t, err)
	assert.Equal(t, bundle, plan.TargetPath)

	staged, err := u.Download(t.Context(), plan, asset, nil)
	require.NoError(t, err)
	require.NoError(t, u.Apply(t.Context(), staged))

	got, err := os.ReadFile(exe)
	require.NoError(t, err)
	assert.Equal(t, payload, got)
	assert.FileExists(t, filepath.Join(bundle, "Contents", "Info.plist"))
	assert.NoDirExists(t, filepath.Join(dir, "__MACOSX"))
}

func TestDownloadRejectsBadChecksum(t *testing.T) {
	httpmock.Activate()
	defer httpmock.DeactivateAndReset()

	dir, target := installDir(t, "EVE Buddy.exe")
	archive := makeZip(t, map[string][]byte{"EVE Buddy.exe": newVersionPayload()})
	asset := serveAsset("evebuddy-1.2.3-windows-x64.zip", archive)
	asset.SHA256 = sha256Hex([]byte("something else"))

	u := newTestUpdater("windows", "amd64", target, "")
	plan, err := u.Plan("1.2.3")
	require.NoError(t, err)

	_, err = u.Download(t.Context(), plan, asset, nil)
	assert.ErrorIs(t, err, ErrChecksumMismatch)

	// The install must be untouched and no staging dir left behind.
	got, err := os.ReadFile(target)
	require.NoError(t, err)
	assert.Equal(t, []byte("OLD VERSION"), got)
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Len(t, entries, 1, "a failed download must not leave a staging dir behind")
}

func TestDownloadAcceptsMissingDigest(t *testing.T) {
	// Older releases predate Github's asset digest field. HTTPS still protects
	// the transfer, so the update proceeds with a warning rather than failing.
	httpmock.Activate()
	defer httpmock.DeactivateAndReset()

	_, target := installDir(t, "EVE Buddy.exe")
	payload := newVersionPayload()
	archive := makeZip(t, map[string][]byte{"EVE Buddy.exe": payload})
	asset := serveAsset("evebuddy-1.2.3-windows-x64.zip", archive)
	asset.SHA256 = ""

	u := newTestUpdater("windows", "amd64", target, "")
	plan, err := u.Plan("1.2.3")
	require.NoError(t, err)
	staged, err := u.Download(t.Context(), plan, asset, nil)
	require.NoError(t, err)
	require.NoError(t, u.Apply(t.Context(), staged))

	got, err := os.ReadFile(target)
	require.NoError(t, err)
	assert.Equal(t, payload, got)
}

func TestDownloadRejectsTruncatedBody(t *testing.T) {
	httpmock.Activate()
	defer httpmock.DeactivateAndReset()

	_, target := installDir(t, "EVE Buddy.exe")
	payload := newVersionPayload()
	archive := makeZip(t, map[string][]byte{"EVE Buddy.exe": payload})
	asset := serveAsset("evebuddy-1.2.3-windows-x64.zip", archive)
	// Claim more bytes than the server will actually send.
	asset.Size = int64(len(archive)) + 1024

	u := newTestUpdater("windows", "amd64", target, "")
	plan, err := u.Plan("1.2.3")
	require.NoError(t, err)
	_, err = u.Download(t.Context(), plan, asset, nil)
	assert.Error(t, err)
}

func TestDownloadRejectsHTTPError(t *testing.T) {
	httpmock.Activate()
	defer httpmock.DeactivateAndReset()

	_, target := installDir(t, "EVE Buddy.exe")
	url := "https://github.com/ErikKalkoken/evebuddy/releases/download/v1.2.3/x.zip"
	httpmock.RegisterResponder("GET", url, httpmock.NewStringResponder(404, "not found"))

	u := newTestUpdater("windows", "amd64", target, "")
	plan, err := u.Plan("1.2.3")
	require.NoError(t, err)
	_, err = u.Download(t.Context(), plan, github.Asset{DownloadURL: url, Name: "x.zip"}, nil)
	assert.ErrorIs(t, err, github.ErrHTTPError)
}

func TestDownloadCanceled(t *testing.T) {
	httpmock.Activate()
	defer httpmock.DeactivateAndReset()

	dir, target := installDir(t, "EVE Buddy.exe")
	archive := makeZip(t, map[string][]byte{"EVE Buddy.exe": newVersionPayload()})
	asset := serveAsset("evebuddy-1.2.3-windows-x64.zip", archive)

	u := newTestUpdater("windows", "amd64", target, "")
	plan, err := u.Plan("1.2.3")
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = u.Download(ctx, plan, asset, nil)
	assert.Error(t, err)

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Len(t, entries, 1, "a canceled download must not leave a staging dir behind")
}

func TestApplyRejectsArchiveWithoutExpectedFile(t *testing.T) {
	httpmock.Activate()
	defer httpmock.DeactivateAndReset()

	_, target := installDir(t, "EVE Buddy.exe")
	// A zip that does not contain the executable we expect.
	archive := makeZip(t, map[string][]byte{"README.txt": []byte("nothing useful")})
	asset := serveAsset("evebuddy-1.2.3-windows-x64.zip", archive)

	u := newTestUpdater("windows", "amd64", target, "")
	plan, err := u.Plan("1.2.3")
	require.NoError(t, err)
	staged, err := u.Download(t.Context(), plan, asset, nil)
	require.NoError(t, err)

	err = u.Apply(t.Context(), staged)
	assert.ErrorIs(t, err, ErrMalformedArchive)

	// The swap must not have started, so the old version is still installed.
	got, err := os.ReadFile(target)
	require.NoError(t, err)
	assert.Equal(t, []byte("OLD VERSION"), got)
	assert.NoFileExists(t, target+oldSuffix)
}

func TestApplyRejectsTruncatedExecutable(t *testing.T) {
	httpmock.Activate()
	defer httpmock.DeactivateAndReset()

	_, target := installDir(t, "EVE Buddy.exe")
	// A zip whose executable is implausibly small, e.g. a failed build.
	archive := makeZip(t, map[string][]byte{"EVE Buddy.exe": []byte("stub")})
	asset := serveAsset("evebuddy-1.2.3-windows-x64.zip", archive)

	u := newTestUpdater("windows", "amd64", target, "")
	plan, err := u.Plan("1.2.3")
	require.NoError(t, err)
	staged, err := u.Download(t.Context(), plan, asset, nil)
	require.NoError(t, err)

	assert.Error(t, u.Apply(t.Context(), staged))
	got, err := os.ReadFile(target)
	require.NoError(t, err)
	assert.Equal(t, []byte("OLD VERSION"), got, "a bad payload must never replace a working install")
}

func TestApplyOverwritesStaleBackup(t *testing.T) {
	httpmock.Activate()
	defer httpmock.DeactivateAndReset()

	_, target := installDir(t, "EVE Buddy.exe")
	// A backup from an earlier update that could not be deleted.
	require.NoError(t, os.WriteFile(target+oldSuffix, []byte("ANCIENT"), 0o755))

	payload := newVersionPayload()
	archive := makeZip(t, map[string][]byte{"EVE Buddy.exe": payload})
	asset := serveAsset("evebuddy-1.2.3-windows-x64.zip", archive)

	u := newTestUpdater("windows", "amd64", target, "")
	plan, err := u.Plan("1.2.3")
	require.NoError(t, err)
	staged, err := u.Download(t.Context(), plan, asset, nil)
	require.NoError(t, err)
	require.NoError(t, u.Apply(t.Context(), staged))

	got, err := os.ReadFile(target)
	require.NoError(t, err)
	assert.Equal(t, payload, got)
}

func TestApplyWithNothingStaged(t *testing.T) {
	_, target := installDir(t, "EVE Buddy.exe")
	u := newTestUpdater("windows", "amd64", target, "")
	err := u.Apply(t.Context(), &Staged{})
	assert.Error(t, err)
}

func TestUnpackZipRejectsPathTraversal(t *testing.T) {
	// A bundle entry that tries to escape the staging directory must be refused.
	dir := t.TempDir()
	archive := filepath.Join(dir, "evil.zip")
	body := makeZip(t, map[string][]byte{
		"EVE Buddy.app/../../escaped.txt": []byte("pwned"),
	})
	require.NoError(t, os.WriteFile(archive, body, 0o644))

	u := newTestUpdater("darwin", "arm64", "/x", "")
	dest := filepath.Join(dir, "dest")
	require.NoError(t, os.Mkdir(dest, 0o755))
	_, err := u.unpackZipTree(t.Context(), archive, dest, "EVE Buddy.app")
	assert.Error(t, err)
	assert.NoFileExists(t, filepath.Join(dir, "escaped.txt"))
}

func TestStagedDiscardIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	s := &Staged{Dir: dir}
	s.Discard()
	assert.NoDirExists(t, dir)
	s.Discard() // must not panic or error
}

func TestProgressWriterThrottles(t *testing.T) {
	var calls int
	w := &progressWriter{
		interval: 0, // report every write
		report:   func(done, total int64) { calls++ },
		total:    100,
	}
	for range 5 {
		_, err := w.Write([]byte("xx"))
		require.NoError(t, err)
	}
	assert.Equal(t, int64(10), w.done)
	assert.Equal(t, 5, calls)

	// With a long interval only the first write reports.
	var throttled int
	w2 := &progressWriter{
		interval: 1e9, // 1 second
		report:   func(done, total int64) { throttled++ },
	}
	for range 5 {
		_, err := w2.Write([]byte("xx"))
		require.NoError(t, err)
	}
	assert.Equal(t, 1, throttled, fmt.Sprintf("expected throttling, got %d calls", throttled))
}

func TestRelaunchCommand(t *testing.T) {
	cases := []struct {
		name     string
		plan     Plan
		wantArgs []string
	}{
		{"windows", Plan{Kind: KindWindowsExe, TargetPath: filepath.Join("apps", "EVE Buddy.exe")}, []string{filepath.Join("apps", "EVE Buddy.exe")}},
		{"linux tar", Plan{Kind: KindLinuxTar, TargetPath: filepath.Join("apps", "evebuddy")}, []string{filepath.Join("apps", "evebuddy")}},
		{"appimage", Plan{Kind: KindAppImage, TargetPath: filepath.Join("apps", "EVE_Buddy.AppImage")}, []string{filepath.Join("apps", "EVE_Buddy.AppImage")}},
		{"mac bundle", Plan{Kind: KindMacBundle, TargetPath: filepath.Join("apps", "EVE Buddy.app")}, []string{"open", "-n", filepath.Join("apps", "EVE Buddy.app")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := relaunchCommand(tc.plan)
			assert.Equal(t, tc.wantArgs, cmd.Args)
			assert.Equal(t, "apps", cmd.Dir)
		})
	}
}

func TestRelaunchWithoutApplyFails(t *testing.T) {
	u := New(Config{})
	assert.Error(t, u.Relaunch())
}
