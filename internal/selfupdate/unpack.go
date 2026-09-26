package selfupdate

import (
	"archive/tar"
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/ulikunitz/xz"
)

// maxUnpackedSize bounds how much is written while unpacking, as a guard
// against a decompression bomb. The largest desktop asset is ~30 MB compressed
// and well under 200 MB unpacked.
const maxUnpackedSize = 1 << 30 // 1 GiB

// ErrMalformedArchive means an archive could not be unpacked, or tried to write
// outside the destination directory.
var ErrMalformedArchive = errors.New("malformed archive")

// unpack extracts the replacement app from a staged download into destDir and
// returns its path, which is a file for every kind except [KindMacBundle].
func (u *Updater) unpack(ctx context.Context, s *Staged, destDir string) (string, error) {
	switch s.Plan.Kind {
	case KindAppImage:
		// An AppImage is not an archive: the asset is the executable itself.
		dest := filepath.Join(destDir, filepath.Base(s.Plan.TargetPath))
		if err := os.Rename(s.ArchivePath, dest); err != nil {
			return "", fmt.Errorf("stage AppImage: %w", err)
		}
		if err := os.Chmod(dest, 0o755); err != nil {
			return "", fmt.Errorf("make AppImage executable: %w", err)
		}
		return dest, nil

	case KindWindowsExe:
		return u.unpackZipFile(ctx, s.ArchivePath, destDir, u.config.ExeName)

	case KindMacBundle:
		return u.unpackZipTree(ctx, s.ArchivePath, destDir, u.config.BundleName)

	case KindLinuxTar:
		return u.unpackTarXZFile(ctx, s.ArchivePath, destDir, u.config.TarBinaryPath)
	}
	return "", ErrUnsupported
}

// unpackZipFile extracts a single named file from the root of a zip archive and
// returns its path.
func (u *Updater) unpackZipFile(ctx context.Context, archive, destDir, name string) (string, error) {
	r, err := zip.OpenReader(archive)
	if err != nil {
		return "", fmt.Errorf("%w: open zip: %w", ErrMalformedArchive, err)
	}
	defer r.Close()
	for _, f := range r.File {
		if err := ensureContext(ctx); err != nil {
			return "", err
		}
		if filepath.Base(filepath.FromSlash(f.Name)) != name || f.FileInfo().IsDir() {
			continue
		}
		dest := filepath.Join(destDir, name)
		if err := writeZipEntry(f, dest, 0o755); err != nil {
			return "", err
		}
		return dest, nil
	}
	return "", fmt.Errorf("%w: %s not found in %s", ErrMalformedArchive, name, filepath.Base(archive))
}

// unpackZipTree extracts a directory tree from a zip archive and returns the
// path of its root. It is used for macOS app bundles, which are directories.
func (u *Updater) unpackZipTree(ctx context.Context, archive, destDir, root string) (string, error) {
	r, err := zip.OpenReader(archive)
	if err != nil {
		return "", fmt.Errorf("%w: open zip: %w", ErrMalformedArchive, err)
	}
	defer r.Close()
	var written int64
	var found bool
	for _, f := range r.File {
		if err := ensureContext(ctx); err != nil {
			return "", err
		}
		name := filepath.FromSlash(f.Name)
		// Only entries inside the expected bundle are extracted, which also
		// discards __MACOSX metadata directories added by some zip tools.
		rel, ok := underRoot(name, root)
		if !ok {
			continue
		}
		found = true
		dest, err := safeJoin(destDir, filepath.Join(root, rel))
		if err != nil {
			return "", err
		}
		info := f.FileInfo()
		switch {
		case info.IsDir():
			if err := os.MkdirAll(dest, 0o755); err != nil {
				return "", err
			}
		case info.Mode()&os.ModeSymlink != 0:
			// App bundles contain symlinks, e.g. inside frameworks.
			if err := writeZipSymlink(f, dest); err != nil {
				return "", err
			}
		default:
			if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
				return "", err
			}
			written += int64(f.UncompressedSize64)
			if written > maxUnpackedSize {
				return "", fmt.Errorf("%w: unpacked size exceeds %d bytes", ErrMalformedArchive, int64(maxUnpackedSize))
			}
			if err := writeZipEntry(f, dest, info.Mode().Perm()); err != nil {
				return "", err
			}
		}
	}
	if !found {
		return "", fmt.Errorf("%w: %s not found in %s", ErrMalformedArchive, root, filepath.Base(archive))
	}
	return filepath.Join(destDir, root), nil
}

// unpackTarXZFile extracts a single file at a known path inside a tar.xz
// archive and returns its path on disk.
func (u *Updater) unpackTarXZFile(ctx context.Context, archive, destDir, path string) (string, error) {
	f, err := os.Open(archive)
	if err != nil {
		return "", err
	}
	defer f.Close()
	xr, err := xz.NewReader(f)
	if err != nil {
		return "", fmt.Errorf("%w: open xz: %w", ErrMalformedArchive, err)
	}
	want := filepath.FromSlash(path)
	tr := tar.NewReader(xr)
	for {
		if err := ensureContext(ctx); err != nil {
			return "", err
		}
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", fmt.Errorf("%w: read tar: %w", ErrMalformedArchive, err)
		}
		// Archive paths may carry a "./" prefix, so they are cleaned first.
		if filepath.Clean(filepath.FromSlash(h.Name)) != want || h.Typeflag != tar.TypeReg {
			continue
		}
		dest := filepath.Join(destDir, filepath.Base(want))
		out, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
		if err != nil {
			return "", err
		}
		defer out.Close()
		if _, err := io.Copy(out, io.LimitReader(tr, maxUnpackedSize)); err != nil {
			return "", fmt.Errorf("extract %s: %w", path, err)
		}
		if err := out.Sync(); err != nil {
			return "", err
		}
		return dest, nil
	}
	return "", fmt.Errorf("%w: %s not found in %s", ErrMalformedArchive, path, filepath.Base(archive))
}

// writeZipEntry writes one zip entry to dest with the given permissions.
func writeZipEntry(f *zip.File, dest string, perm os.FileMode) error {
	rc, err := f.Open()
	if err != nil {
		return fmt.Errorf("%w: open %s: %w", ErrMalformedArchive, f.Name, err)
	}
	defer rc.Close()
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, io.LimitReader(rc, maxUnpackedSize)); err != nil {
		return fmt.Errorf("extract %s: %w", f.Name, err)
	}
	return out.Sync()
}

// writeZipSymlink recreates a symlink stored in a zip archive. The link target
// is the entry's content.
func writeZipSymlink(f *zip.File, dest string) error {
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	target, err := io.ReadAll(io.LimitReader(rc, 4096))
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	// A pre-existing link would make Symlink fail, e.g. on a retried unpack.
	if err := os.Remove(dest); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.Symlink(string(target), dest)
}

// underRoot reports whether an archive path lies inside root, and returns the
// remainder of the path relative to it.
func underRoot(name, root string) (string, bool) {
	name = filepath.Clean(name)
	if name == root {
		return "", true
	}
	rest, ok := strings.CutPrefix(name, root+string(filepath.Separator))
	if !ok {
		return "", false
	}
	return rest, true
}

// safeJoin joins a relative archive path onto a base directory and rejects any
// path that would escape it, e.g. via "..". See CVE-2018-1000116 ("zip slip").
func safeJoin(base, rel string) (string, error) {
	dest := filepath.Join(base, rel)
	// filepath.Join cleans the result, so a contained path always keeps the
	// base plus a separator as its prefix.
	if dest != base && !strings.HasPrefix(dest, base+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: entry escapes destination: %s", ErrMalformedArchive, rel)
	}
	return dest, nil
}
