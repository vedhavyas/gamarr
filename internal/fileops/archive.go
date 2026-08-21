package fileops

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// TarExt is the extension of a vault archive.
const TarExt = ".tar"

// partialSuffix marks an archive that is still being written.
const partialSuffix = ".partial"

// ErrArchiveExists is returned rather than replacing an archive already in the
// vault. Publishing an archive is an atomic and total rename, unlike the
// directory merge it replaced, so overwriting would discard a complete game
// with nothing left to notice it by.
var ErrArchiveExists = errors.New("an archive already exists at the destination")

// ArchiveDest returns the archive path for a vault destination.
func ArchiveDest(dest string) string { return dest + TarExt }

// partialPattern is the os.CreateTemp pattern for an in-progress archive. The
// leading dot is load-bearing: it keeps a partial out of Gamarr's own vault
// scan, which skips dotfiles, and out of a GameVault library scan.
func partialPattern(dest string) string {
	return "." + filepath.Base(dest) + ".*" + partialSuffix
}

// archiveLocks serializes archives by destination. Checking that the
// destination is free and then publishing are two steps, so without this two
// imports resolving to the same vault name both pass the check and one replaces
// the other. Keyed by path, not by torrent hash: the same game can arrive over
// both a torrent and an NZB.
var archiveLocks sync.Map

func lockDest(dest string) func() {
	v, _ := archiveLocks.LoadOrStore(dest, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// Archivable reports whether src is worth writing as an archive. Only a real
// directory is: a single file already is the one-file-per-game unit a GameVault
// library indexes.
//
// Lstat rather than Stat, because Stat follows a symlink to a directory and
// would report one here, while the walk that writes the archive lstats the same
// root, sees a link, and descends into nothing.
func Archivable(src string) bool {
	fi, err := os.Lstat(src)
	return err == nil && fi.IsDir()
}

// Archive writes the tree at src into a single uncompressed tar at dest,
// leaving src in place.
//
// The tar is a pure container, never compressed: a repack's payload is already
// compressed, so a second pass costs hours of CPU for no size change.
//
// A nil return means every regular file under src is in the archive and the
// archive is on the backing store under its final name. Callers that delete the
// source afterwards depend on precisely that, and a successful rename(2) does
// not imply it, so the result is checked against a pre-walk census and synced
// before it is published.
func Archive(src, dest string) error {
	defer lockDest(dest)()

	wantFiles, wantBytes, err := census(src)
	if err != nil {
		return err
	}
	if wantFiles == 0 {
		return fmt.Errorf("refusing to archive %s: it holds no regular files", src)
	}
	if _, err := os.Lstat(dest); err == nil {
		return fmt.Errorf("%w: %s", ErrArchiveExists, dest)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
		return err
	}

	// CreateTemp is O_EXCL, so two imports resolving to the same vault name
	// cannot open one inode and interleave into each other's entries.
	f, err := os.CreateTemp(filepath.Dir(dest), partialPattern(dest))
	if err != nil {
		return err
	}
	partial := f.Name()

	gotFiles, gotBytes, err := writeTar(f, src)
	if err == nil && (gotFiles != wantFiles || gotBytes != wantBytes) {
		err = fmt.Errorf("archive of %s holds %d files and %d bytes, source has %d and %d",
			src, gotFiles, gotBytes, wantFiles, wantBytes)
	}
	if err == nil {
		// The vault can be a cache in front of a remote, where close(2) returns
		// long before the bytes are durable and the writeback error arrives
		// after this function has already said yes.
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(partial, dest)
	}
	if err != nil {
		removePartial(partial)
		return err
	}
	return nil
}

// removePartial drops an abandoned partial. Each attempt gets a unique name, so
// nothing reuses it and a failure here leaks disk until the next sweep.
func removePartial(path string) {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		slog.Error("leaked a partial vault archive", "path", path, "error", err)
	}
}

// SweepPartials removes partial archives abandoned in dir by a process that
// died mid-write, returning how many it dropped. minAge keeps it clear of an
// archive still being written.
func SweepPartials(dir string, minAge time.Duration) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	cutoff := time.Now().Add(-minAge)
	removed := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, ".") || !strings.HasSuffix(name, partialSuffix) {
			continue
		}
		info, err := e.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		path := filepath.Join(dir, name)
		if err := os.Remove(path); err != nil {
			slog.Warn("could not remove an abandoned partial archive", "path", path, "error", err)
			continue
		}
		slog.Info("removed an abandoned partial archive", "path", path)
		removed++
	}
	return removed
}

// census counts the regular files under src and their total size: the figures a
// finished archive has to match before it can stand in for the source.
func census(src string) (files, bytes int64, err error) {
	err = filepath.Walk(src, func(_ string, info fs.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			files++
			bytes += info.Size()
		}
		return nil
	})
	return files, bytes, err
}

func writeTar(f *os.File, src string) (files, bytes int64, err error) {
	tw := tar.NewWriter(f)
	files, bytes, err = writeTree(tw, src)
	// tar buffers, so a short write on the final entry surfaces at Close.
	if cerr := tw.Close(); err == nil {
		err = cerr
	}
	return files, bytes, err
}

// writeTree adds every file under src to tw, named relative to src, and reports
// what it wrote so the caller can compare against the source.
func writeTree(tw *tar.Writer, src string) (files, bytes int64, err error) {
	err = filepath.Walk(src, func(path string, info fs.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		// Redundant, since rel comes from Rel over Walk output and can be
		// neither absolute nor contain "..", but it costs nothing.
		if !filepath.IsLocal(rel) {
			return fmt.Errorf("unsafe path %q escapes %q", rel, src)
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			// Skipping one and returning success is the worse failure: on the
			// usenet path a nil return authorizes deleting the source.
			return fmt.Errorf("refusing to archive %s: %s is not a regular file", src, rel)
		}

		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		if info.IsDir() {
			hdr.Name += "/"
			return tw.WriteHeader(hdr)
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}

		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		// tar declares each entry's size up front, so a file that changed size
		// since the walk fails the write rather than landing truncated.
		n, err := io.Copy(tw, in)
		if err != nil {
			return err
		}
		files++
		bytes += n
		return nil
	})
	return files, bytes, err
}
