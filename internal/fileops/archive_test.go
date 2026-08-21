package fileops

import (
	"archive/tar"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
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

// vaultEntries lists what is visible in a vault directory.
func vaultEntries(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestArchiveDest(t *testing.T) {
	// GameVault strips a (...) group to parse the release year, so the year has
	// to survive as part of the name.
	got := ArchiveDest(filepath.Join("/vault", "007 First Light (2025)"))
	want := filepath.Join("/vault", "007 First Light (2025).tar")
	if got != want {
		t.Errorf("ArchiveDest = %q, want %q", got, want)
	}
}

// The dot is what keeps an in-progress archive out of Gamarr's vault scan,
// which skips dotfiles, and out of a GameVault scan.
func TestPartialPatternIsHidden(t *testing.T) {
	p := partialPattern(filepath.Join("/vault", "Game.tar"))
	if !strings.HasPrefix(p, ".") {
		t.Errorf("partial pattern %q is not hidden", p)
	}
	if !strings.HasSuffix(p, partialSuffix) {
		t.Errorf("partial pattern %q does not end in %q", p, partialSuffix)
	}
}

func TestArchivable(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "Game")
	writeFile(t, filepath.Join(dir, "setup.exe"), "SETUP")
	file := filepath.Join(root, "loose.exe")
	writeFile(t, file, "LOOSE")
	link := filepath.Join(root, "linked")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	if !Archivable(dir) {
		t.Error("a directory should be archivable")
	}
	if Archivable(file) {
		t.Error("a single file is already one file per game, so should not be archivable")
	}
	// Stat would follow this and call it a directory, while the walk that writes
	// the archive lstats the same root and descends into nothing.
	if Archivable(link) {
		t.Error("a symlink to a directory should not be archivable")
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

	vault := filepath.Join(root, "vault")
	dest := ArchiveDest(filepath.Join(vault, "007 First Light"))
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

	if names := vaultEntries(t, vault); len(names) != 1 || names[0] != "007 First Light.tar" {
		t.Errorf("vault holds %v, want just the archive", names)
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

// The only failure the deletion gate has to survive is one after the tar is
// part-written. Walk order puts the readable file first, so bytes reach the
// partial before the symlink fails the archive.
func TestArchiveFailingMidWriteLeavesNothingBehind(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "Game")
	writeFile(t, filepath.Join(src, "a-payload.bin"), strings.Repeat("PAYLOAD!", 8192))
	if err := os.Symlink(filepath.Join(src, "a-payload.bin"), filepath.Join(src, "zz-link")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	vault := filepath.Join(root, "vault")
	dest := ArchiveDest(filepath.Join(vault, "Game"))
	err := Archive(src, dest)
	if err == nil {
		t.Fatal("want an error when the tree holds an irregular file")
	}
	// Names the symlink, which proves the regular file ahead of it was written
	// before the failure rather than the archive failing up front.
	if !strings.Contains(err.Error(), "zz-link") {
		t.Errorf("error = %v, want it to name the irregular entry", err)
	}

	if _, statErr := os.Lstat(dest); statErr == nil {
		t.Error("a failed archive was published to the vault")
	}
	if names := vaultEntries(t, vault); len(names) != 0 {
		t.Errorf("vault holds %v after a failed archive, want nothing", names)
	}
}

// Each of these returned a 1024-byte tar and nil before, which on the usenet
// path then authorized deleting the only copy of the download.
func TestArchiveRefusesAnEmptyResult(t *testing.T) {
	root := t.TempDir()

	empty := filepath.Join(root, "Empty")
	if err := os.MkdirAll(filepath.Join(empty, "nested"), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	realDir := filepath.Join(root, "Real")
	writeFile(t, filepath.Join(realDir, "setup.exe"), "SETUP")
	linked := filepath.Join(root, "Linked")
	if err := os.Symlink(realDir, linked); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	for _, src := range []string{empty, linked} {
		dest := ArchiveDest(filepath.Join(root, "vault", filepath.Base(src)))
		if err := Archive(src, dest); err == nil {
			t.Errorf("Archive(%s) = nil, want an error rather than an empty tar", src)
		}
		if _, err := os.Lstat(dest); err == nil {
			t.Errorf("Archive(%s) published an archive anyway", src)
		}
	}
}

// A rename overwrites atomically and totally, unlike the directory merge the
// archive path replaced, so a second import must not silently replace a
// complete game.
func TestArchiveRefusesAnExistingDestination(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "Game")
	writeFile(t, filepath.Join(src, "setup.exe"), "SETUP")

	dest := ArchiveDest(filepath.Join(root, "vault", "Game"))
	if err := Archive(src, dest); err != nil {
		t.Fatalf("first Archive: %v", err)
	}
	before := read(t, dest)

	err := Archive(src, dest)
	if !errors.Is(err, ErrArchiveExists) {
		t.Errorf("second Archive = %v, want ErrArchiveExists", err)
	}
	if read(t, dest) != before {
		t.Error("the existing archive was modified")
	}
}

// Two flows can resolve to one vault name: a torrent and an NZB of the same
// game, or a re-grab. Exactly one may publish, and what it publishes has to be
// one source in full rather than a splice of both.
func TestArchiveConcurrentSameDestination(t *testing.T) {
	root := t.TempDir()
	sources := map[string]string{"A": strings.Repeat("AAAA", 4096), "B": strings.Repeat("BBBB", 4096)}
	for name, content := range sources {
		writeFile(t, filepath.Join(root, "src"+name, "payload.bin"), content)
	}

	dest := ArchiveDest(filepath.Join(root, "vault", "Game"))
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, name := range []string{"A", "B"} {
		wg.Add(1)
		go func(i int, name string) {
			defer wg.Done()
			errs[i] = Archive(filepath.Join(root, "src"+name), dest)
		}(i, name)
	}
	wg.Wait()

	okCount := 0
	for _, err := range errs {
		if err == nil {
			okCount++
		}
	}
	if okCount != 1 {
		t.Fatalf("errs = %v, want exactly one success", errs)
	}

	got := readTar(t, dest)
	body, ok := got["payload.bin"]
	if !ok {
		t.Fatal("published archive has no payload entry")
	}
	if body != sources["A"] && body != sources["B"] {
		t.Errorf("published archive holds %d bytes matching neither source in full", len(body))
	}
	if names := vaultEntries(t, filepath.Dir(dest)); len(names) != 1 {
		t.Errorf("vault holds %v, want just the archive", names)
	}
}

func TestArchiveMissingSource(t *testing.T) {
	root := t.TempDir()
	err := Archive(filepath.Join(root, "nope"), ArchiveDest(filepath.Join(root, "vault", "nope")))
	if err == nil {
		t.Fatal("want an error archiving a source that does not exist")
	}
}

// A partial abandoned by a crash gets a unique name, so nothing reclaims it.
func TestSweepPartials(t *testing.T) {
	vault := t.TempDir()
	stale := filepath.Join(vault, ".Game.tar.123"+partialSuffix)
	fresh := filepath.Join(vault, ".Other.tar.456"+partialSuffix)
	keep := filepath.Join(vault, "Real Game.tar")
	for _, p := range []string{stale, fresh, keep} {
		writeFile(t, p, "x")
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	if n := SweepPartials(vault, 24*time.Hour); n != 1 {
		t.Errorf("swept %d, want 1", n)
	}
	if _, err := os.Lstat(stale); err == nil {
		t.Error("the abandoned partial survived")
	}
	// A partial younger than the floor may belong to an import still writing.
	if _, err := os.Lstat(fresh); err != nil {
		t.Error("a recent partial was swept")
	}
	if _, err := os.Lstat(keep); err != nil {
		t.Error("a real archive was swept")
	}
}
