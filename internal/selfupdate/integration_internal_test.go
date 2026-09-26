package selfupdate

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ErikKalkoken/evebuddy/internal/github"
)

// integrationEnv gates the integration test below. It is opt-in rather than
// skipped in short mode, so that neither CI nor a plain "go test ./..." ever
// downloads a release.
const integrationEnv = "EVEBUDDY_SELFUPDATE_INTEGRATION"

// TestIntegrationRealRelease downloads the current real release from Github and
// applies it to a throwaway installation, then runs the result to confirm the
// swapped-in file is a working executable of the expected version.
//
// It needs network access and downloads ~30 MB, so it only runs when
// EVEBUDDY_SELFUPDATE_INTEGRATION is set:
//
//	EVEBUDDY_SELFUPDATE_INTEGRATION=1 go test -tags migrated_fynedo -run Integration ./internal/selfupdate/
func TestIntegrationRealRelease(t *testing.T) {
	if os.Getenv(integrationEnv) == "" {
		t.Skipf("set %s to run this test; it downloads a real release from Github", integrationEnv)
	}
	if runtime.GOOS != "windows" {
		t.Skip("this test covers the Windows exe swap")
	}

	release, err := github.LatestRelease(t.Context(), "ErikKalkoken", "evebuddy")
	require.NoError(t, err)
	t.Logf("latest release: %s (version %s, %d assets)", release.TagName, release.Version, len(release.Assets))

	// A throwaway installation standing in for a real one.
	dir := t.TempDir()
	target := filepath.Join(dir, "EVE Buddy.exe")
	require.NoError(t, os.WriteFile(target, []byte("PRETEND OLD VERSION"), 0o755))

	u := New(Config{
		AppImageBaseName: "EVE_Buddy",
		AssetBaseName:    "evebuddy",
		BundleName:       "EVE Buddy.app",
		ExeName:          "EVE Buddy.exe",
		TarBinaryPath:    "usr/local/bin/evebuddy",
	})
	// Point the updater at the throwaway install rather than the test binary,
	// and disable the transient location guard, since t.TempDir is under the
	// system temporary directory that guard exists to reject.
	u.executablePath = func() (string, error) { return target, nil }
	u.tempDir = func() string { return filepath.Join(string(filepath.Separator), "no-such-temp-dir") }

	plan, err := u.Plan(release.Version)
	require.NoError(t, err)
	t.Logf("plan: kind=%s asset=%s", plan.Kind, plan.AssetName)

	asset, ok := release.Asset(plan.AssetName)
	require.True(t, ok, "release %s must contain %s", release.TagName, plan.AssetName)
	require.NotEmpty(t, asset.SHA256, "Github must report a digest for the asset")
	t.Logf("asset: %s (%d bytes) sha256=%s", asset.Name, asset.Size, asset.SHA256)

	var reports int
	staged, err := u.Download(t.Context(), plan, asset, func(done, total int64) {
		reports++
	})
	require.NoError(t, err, "download and checksum verification must succeed")
	t.Logf("downloaded and verified after %d progress reports", reports)

	require.NoError(t, u.Apply(t.Context(), staged))

	fi, err := os.Stat(target)
	require.NoError(t, err)
	assert.Greater(t, fi.Size(), int64(10<<20), "the installed exe must be a real binary")

	// The decisive check: the swapped-in file must actually run.
	out, err := exec.Command(target, "-v").Output()
	require.NoError(t, err, "the swapped-in executable must run")
	assert.Equal(t, release.Version, strings.TrimSpace(string(out)),
		"the installed exe must report the new version")

	// The replaced executable is kept, because Windows can not delete it while
	// it is running, and removed by the next Cleanup.
	assert.FileExists(t, target+oldSuffix)
	u.Cleanup()
	assert.NoFileExists(t, target+oldSuffix)
	assert.FileExists(t, target)
}
