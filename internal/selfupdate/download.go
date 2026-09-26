package selfupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ErikKalkoken/evebuddy/internal/github"
)

// downloadTimeout bounds a whole download. The release assets are ~30 MB, so
// this is generous even on a slow connection, while still preventing a stalled
// transfer from hanging a progress dialog forever.
const downloadTimeout = 30 * time.Minute

// progressReportInterval throttles progress callbacks, which are delivered to
// the UI thread and would otherwise fire thousands of times per second.
const progressReportInterval = 100 * time.Millisecond

// Staged is a downloaded and verified update, ready to be applied.
type Staged struct {
	// ArchivePath is the downloaded asset.
	ArchivePath string
	// Dir is the staging directory holding the download. It is created next to
	// the installed app so that the final swap is a rename on the same volume
	// rather than a copy across volumes, which would not be atomic.
	Dir string
	// Plan is the update this download belongs to.
	Plan Plan
}

// Discard removes a staged download. It is safe to call more than once and
// should be called when an update is not applied after all.
func (s *Staged) Discard() {
	if s.Dir == "" {
		return
	}
	if err := os.RemoveAll(s.Dir); err != nil {
		slog.Warn("selfupdate: failed to discard staged update", "path", s.Dir, "error", err)
	}
	s.Dir = ""
}

// Download fetches a release asset into a staging directory next to the
// installed app and verifies it against the digest reported by Github.
//
// progress, when not nil, is called with the number of bytes downloaded and the
// total expected. It is called from the calling goroutine, so a UI caller must
// marshal onto the UI thread itself. total is 0 when the size is unknown.
//
// The returned [Staged] must be applied with [Updater.Apply] or released with
// [Staged.Discard].
func (u *Updater) Download(ctx context.Context, p Plan, a github.Asset, progress func(done, total int64)) (*Staged, error) {
	if a.DownloadURL == "" {
		return nil, fmt.Errorf("%w: %s", ErrAssetMissing, p.AssetName)
	}
	// Check before creating a staging dir, so an already canceled update leaves
	// nothing behind at all.
	if err := ensureContext(ctx); err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp(filepath.Dir(p.TargetPath), stagingPrefix+"*")
	if err != nil {
		return nil, fmt.Errorf("%w: create staging dir: %w", ErrNotWritable, err)
	}
	staged := &Staged{Dir: dir, Plan: p}
	// Any failure past this point must not leave a partial download behind.
	defer func() {
		if err != nil {
			staged.Discard()
		}
	}()

	archive := filepath.Join(dir, sanitizeFileName(a.Name))
	if err = u.download(ctx, a, archive, progress); err != nil {
		return nil, err
	}
	staged.ArchivePath = archive
	slog.Info("selfupdate: download complete", "asset", a.Name, "path", archive)
	return staged, nil
}

func (u *Updater) download(ctx context.Context, a github.Asset, dest string, progress func(done, total int64)) error {
	ctx, cancel := context.WithTimeout(ctx, downloadTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.DownloadURL, nil)
	if err != nil {
		return err
	}
	// A dedicated client is used rather than the app's shared, cached client:
	// a 30 MB binary must never enter the HTTP cache, and the shared client's
	// short timeout would abort the transfer.
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("download %s: %w", a.Name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("download %s: %s: %w", a.Name, resp.Status, github.ErrHTTPError)
	}

	total := a.Size
	if total == 0 && resp.ContentLength > 0 {
		total = resp.ContentLength
	}

	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer f.Close()

	hash := sha256.New()
	counter := &progressWriter{
		interval: progressReportInterval,
		report:   progress,
		total:    total,
	}
	// Hash while writing, so the file is read only once.
	if _, err := io.Copy(io.MultiWriter(f, hash, counter), resp.Body); err != nil {
		return fmt.Errorf("download %s: %w", a.Name, err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("flush %s: %w", dest, err)
	}
	// Report the final state, which throttling may have skipped.
	if progress != nil {
		progress(counter.done, total)
	}

	if total > 0 && counter.done != total {
		return fmt.Errorf("download %s: got %d bytes, expected %d", a.Name, counter.done, total)
	}
	if a.SHA256 == "" {
		// Github populates a digest for all recent assets, but an older release
		// may predate the field. HTTPS still protects the transfer, so this is
		// downgraded to a warning rather than refusing the update.
		slog.Warn("selfupdate: release asset has no digest, skipping verification", "asset", a.Name)
		return nil
	}
	got := hex.EncodeToString(hash.Sum(nil))
	if !strings.EqualFold(got, a.SHA256) {
		return fmt.Errorf("%w: %s: got %s, expected %s", ErrChecksumMismatch, a.Name, got, a.SHA256)
	}
	slog.Info("selfupdate: verified asset checksum", "asset", a.Name, "sha256", got)
	return nil
}

// progressWriter counts bytes written and reports progress at a fixed interval.
type progressWriter struct {
	done     int64
	interval time.Duration
	last     time.Time
	report   func(done, total int64)
	total    int64
}

func (w *progressWriter) Write(p []byte) (int, error) {
	w.done += int64(len(p))
	if w.report == nil {
		return len(p), nil
	}
	if now := time.Now(); now.Sub(w.last) >= w.interval {
		w.last = now
		w.report(w.done, w.total)
	}
	return len(p), nil
}

// sanitizeFileName reduces a name from an untrusted source to a plain file name,
// so that it can not escape the staging directory.
func sanitizeFileName(name string) string {
	name = filepath.Base(filepath.FromSlash(name))
	if name == "." || name == string(filepath.Separator) || name == ".." {
		return "download"
	}
	return name
}
