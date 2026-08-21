package fileops

import (
	"archive/tar"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
)

// TarExt is the extension of a vault archive.
const TarExt = ".tar"

// ArchiveDest returns the archive path for a vault destination.
func ArchiveDest(dest string) string { return dest + TarExt }

// Archivable reports whether src is worth writing as an archive. Only a
// directory is: a single file already is the one-file-per-game unit a
// GameVault library indexes.
func Archivable(src string) bool {
	fi, err := os.Stat(src)
	return err == nil && fi.IsDir()
}

// Archive writes the tree at src into a single uncompressed tar at dest,
// named relative to src and leaving src in place.
//
// The tar is a pure container, never compressed: a repack's payload is
// already compressed, so a second pass costs hours of CPU for no size change.
func Archive(src, dest string) error {
	if _, err := os.Stat(src); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
		return err
	}

	// A half-written tar must not read as a finished game to Gamarr's vault
	// scan or to GameVault's; the leading dot keeps it out of both.
	partial := filepath.Join(filepath.Dir(dest), "."+filepath.Base(dest)+".partial")
	f, err := os.Create(partial)
	if err != nil {
		return err
	}

	tw := tar.NewWriter(f)
	err = writeTree(tw, src)
	if cerr := tw.Close(); err == nil {
		err = cerr
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(partial)
		return err
	}
	if err := os.Rename(partial, dest); err != nil {
		os.Remove(partial)
		return err
	}
	return nil
}

// writeTree adds every file under src to tw, named relative to src.
func writeTree(tw *tar.Writer, src string) error {
	return filepath.Walk(src, func(path string, info fs.FileInfo, err error) error {
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
		if !filepath.IsLocal(rel) {
			return fmt.Errorf("unsafe path %q escapes %q", rel, src)
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			slog.Warn("skipping irregular file in vault archive", "path", path)
			return nil
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
		_, err = io.Copy(tw, in)
		return err
	})
}
