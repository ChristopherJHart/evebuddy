package selfupdate

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

// Apply unpacks a staged download and swaps it into place, replacing the
// installed app.
//
// The swap uses renames only, never a copy, so the installed app is either the
// old version or the new one and never a half-written file. The old app is kept
// alongside the new one with an [oldSuffix] suffix, because on Windows a running
// executable can be renamed but not deleted; it is removed by [Updater.Cleanup]
// on the next start.
//
// Apply does not restart the app. Call [Relaunch] after shutdown.
func (u *Updater) Apply(ctx context.Context, s *Staged) error {
	if s.Dir == "" || s.ArchivePath == "" {
		return fmt.Errorf("apply update: nothing staged")
	}
	target := s.Plan.TargetPath

	replacement, err := u.unpack(ctx, s, s.Dir)
	if err != nil {
		return err
	}
	// Refuse to swap in something implausible. Without this check a truncated or
	// empty extraction would replace a working install with a broken one.
	if err := verifyReplacement(replacement, s.Plan.Kind); err != nil {
		return err
	}

	old := target + oldSuffix
	// A leftover from a previous update would make the first rename fail.
	if err := os.RemoveAll(old); err != nil {
		slog.Debug("selfupdate: could not remove stale backup", "path", old, "error", err)
	}

	// Step 1: move the installed app aside. On Windows this is permitted even
	// though the file is running and locked against writes and deletes.
	if err := os.Rename(target, old); err != nil {
		return fmt.Errorf("move current version aside: %w", err)
	}
	// Step 2: move the new app into place. The window between the two renames is
	// the only point at which the app is not installed, so nothing else happens
	// in between. On failure the first rename is undone.
	if err := os.Rename(replacement, target); err != nil {
		if rollbackErr := os.Rename(old, target); rollbackErr != nil {
			// Both the swap and its rollback failed. Say exactly where the app
			// went, because the user now has to restore it by hand.
			return fmt.Errorf(
				"install new version: %w; rollback also failed: %w; "+
					"the previous version is still available at %s and must be renamed back to %s manually",
				err, rollbackErr, old, target,
			)
		}
		return fmt.Errorf("install new version, previous version restored: %w", err)
	}
	slog.Info("selfupdate: applied update", "version", s.Plan.Version, "target", target, "previous", old)

	// The staging dir is now empty apart from the archive. Removing it here
	// keeps the install directory clean even if the app never restarts.
	s.Discard()

	if runtime.GOOS != "windows" {
		// Unlinking a running binary is fine on Unix, so the backup can go at
		// once rather than waiting for the next start.
		if err := os.RemoveAll(old); err != nil {
			slog.Debug("selfupdate: could not remove previous version", "path", old, "error", err)
		}
	}
	return nil
}

// verifyReplacement sanity checks an unpacked app before it replaces the
// installed one.
func verifyReplacement(path string, k Kind) error {
	fi, err := os.Stat(path)
	if err != nil {
		// Most likely cause on Windows: a virus scanner removed the extracted
		// executable, which the README notes happens with false positives.
		return fmt.Errorf("verify new version: %w", err)
	}
	if k == KindMacBundle {
		if !fi.IsDir() {
			return fmt.Errorf("verify new version: %s is not an app bundle", path)
		}
		return nil
	}
	if fi.IsDir() {
		return fmt.Errorf("verify new version: %s is a directory", path)
	}
	// Every desktop asset is tens of MB; anything tiny is a failed extraction.
	const minSize = 1 << 20 // 1 MiB
	if fi.Size() < minSize {
		return fmt.Errorf("verify new version: %s is only %d bytes", path, fi.Size())
	}
	return nil
}

// Relaunch starts a new instance of the installed app and returns immediately.
//
// It must only be called once the app has fully shut down. Two things make this
// strict: the single instance mutex is still held until then, and a second
// instance that finds the mutex taken will simply raise the running window and
// exit, so an early relaunch silently does nothing.
func Relaunch() error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate executable for relaunch: %w", err)
	}
	// os.Executable is resolved before the swap on Windows, where the running
	// image keeps its original path; after the swap that path holds the new
	// version, which is what should start.
	cmd := exec.Command(exe)
	cmd.Dir = filepath.Dir(exe)
	// Detach from this process so the new instance survives our exit and does
	// not inherit our standard streams.
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("relaunch %s: %w", exe, err)
	}
	// Release the child so it is not tied to this process' lifetime.
	if err := cmd.Process.Release(); err != nil {
		slog.Warn("selfupdate: failed to release relaunched process", "error", err)
	}
	slog.Info("selfupdate: relaunched", "path", exe, "pid", cmd.Process.Pid)
	return nil
}
