package fileops

import (
	"archive/tar"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// readTar returns every entry in a tar keyed by name, directories included as
// empty strings.
func readTar(t *testing.T, path string) map[string]string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()

	out := map[string]string{}
	tr := tar.NewReader(f)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read tar %s: %v", path, err)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("read entry %s: %v", hdr.Name, err)
		}
		out[hdr.Name] = string(body)
	}
	return out
}

func TestArchiveDest(t *testing.T) {
	// GameVault strips a (...) group to parse the release year, so the year
	// has to survive as part of the name.
	got := ArchiveDest(filepath.Join("/vault", "007 First Light (2025)"))
	want := filepath.Join("/vault", "007 First Light (2025).tar")
	if got != want {
		t.Errorf("ArchiveDest = %q, want %q", got, want)
	}
}

func TestArchivable(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "Game")
	writeFile(t, filepath.Join(dir, "setup.exe"), "SETUP")
	file := filepath.Join(root, "loose.exe")
	writeFile(t, file, "LOOSE")

	if !Archivable(dir) {
		t.Error("a directory should be archivable")
	}
	if Archivable(file) {
		t.Error("a single file is already one file per game, so should not be archivable")
	}
	if Archivable(filepath.Join(root, "missing")) {
		t.Error("a missing path should not be archivable")
	}
}

// A repack arrives as a folder of parts. All of them have to reach the tar,
// nested ones included, or the archive is a game that cannot be installed.
func TestArchiveHoldsEveryInputFile(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "007 First Light")
	want := map[string]string{
		"setup.exe":            "SETUP",
		"fg-01.bin":            "PART ONE",
		"fg-02.bin":            "PART TWO",
		"MD5/fitgirl-bins.md5": "HASHES",
	}
	for name, content := range want {
		writeFile(t, filepath.Join(src, name), content)
	}

	dest := ArchiveDest(filepath.Join(root, "vault", "007 First Light"))
	if err := Archive(src, dest); err != nil {
		t.Fatalf("Archive: %v", err)
	}

	got := readTar(t, dest)
	for name, content := range want {
		if got[name] != content {
			t.Errorf("tar entry %q = %q, want %q", name, got[name], content)
		}
	}
	delete(got, "MD5/")
	if len(got) != len(want) {
		t.Errorf("tar holds %d entries, want %d: %v", len(got), len(want), got)
	}

	// A tar cut short by a full disk must not sit in the vault looking like a
	// finished game, so nothing may be left beside the finished archive.
	entries, err := os.ReadDir(filepath.Dir(dest))
	if err != nil {
		t.Fatalf("read vault: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "007 First Light.tar" {
		t.Errorf("vault holds %d entries, want just the archive", len(entries))
	}
}

// Archiving reads the source and writes a new file, so it can never be a move:
// a torrent stays seedable and IMPORT_MODE cannot make an archive delete data.
func TestArchiveLeavesSourceInPlace(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "Game")
	writeFile(t, filepath.Join(src, "setup.exe"), "SETUP")

	dest := ArchiveDest(filepath.Join(root, "vault", "Game"))
	if err := Archive(src, dest); err != nil {
		t.Fatalf("Archive: %v", err)
	}

	if read(t, filepath.Join(src, "setup.exe")) != "SETUP" {
		t.Error("source file did not survive the archive")
	}
}

func TestArchiveMissingSource(t *testing.T) {
	root := t.TempDir()
	err := Archive(filepath.Join(root, "nope"), ArchiveDest(filepath.Join(root, "vault", "nope")))
	if err == nil {
		t.Fatal("want an error archiving a source that does not exist")
	}
}
