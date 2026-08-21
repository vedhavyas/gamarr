package download

import (
	"archive/tar"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gamarr/internal/qbit"
)

// tarEntries returns a vault archive's contents keyed by entry name. Asserting
// the archive merely exists cannot tell "archived the download" from "archived
// nothing", and on the usenet path the second one deletes the download.
func tarEntries(t *testing.T, path string) map[string]string {
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

// irregularTree builds a source dir whose first entry archives cleanly and
// whose second fails, so Archive errors after bytes have been written.
func irregularTree(t *testing.T, dir string) {
	t.Helper()
	writeFileT(t, filepath.Join(dir, "a-payload.bin"), []byte(strings.Repeat("PAYLOAD!", 8192)))
	if err := os.Symlink(filepath.Join(dir, "a-payload.bin"), filepath.Join(dir, "zz-link")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
}

// GameVault indexes one file per game, so a repack folder has to reach the
// vault as a single tar rather than as the folder the torrent named.
func TestOrganizeGameArchivesVaultFolder(t *testing.T) {
	cfg := newTestConfig(t)
	cfg.VaultArchiveEnabled = true
	jobs := newTestJobs(t)

	content := filepath.Join(t.TempDir(), "007 First Light (2025)")
	writeFileT(t, filepath.Join(content, "setup.exe"), []byte("installer"))
	writeFileT(t, filepath.Join(content, "fg-01.bin"), []byte("payload"))

	qm := newQbitMock(t)
	m := New(cfg, jobs, qm.client())
	jobID := newJobID()
	jobs.Set(jobID, map[string]interface{}{"status": "organizing", "error": nil})

	torrent := &qbit.Torrent{Name: "007 First Light (2025)", Hash: "h1", ContentPath: content}
	m.organizeGame(jobID, torrent, "PC", "", true)

	dest := filepath.Join(cfg.GamesVaultPath, "007 First Light (2025).tar")
	got := tarEntries(t, dest)
	if got["setup.exe"] != "installer" || got["fg-01.bin"] != "payload" {
		t.Errorf("archive contents = %v, want both source files", got)
	}
	if pathExists(filepath.Join(cfg.GamesVaultPath, "007 First Light (2025)")) {
		t.Error("the folder was written to the vault as well as the archive")
	}
	if !m.Jobs().LibraryHasSourceID("torrent:h1") {
		t.Error("archive was not tracked in the library")
	}

	// An archive is a copy, so the seeded data must survive and the torrent must
	// not be deleted with its files.
	if !pathExists(filepath.Join(content, "fg-01.bin")) {
		t.Error("archiving destroyed the source")
	}
	for _, call := range qm.deleteCalls() {
		if call.deleteFiles {
			t.Errorf("torrent %s was deleted with its data after an archive import", call.hash)
		}
	}
}

func TestOrganizeNZBArchivesVaultFolder(t *testing.T) {
	cfg := newTestConfig(t)
	cfg.VaultArchiveEnabled = true
	m := New(cfg, newTestJobs(t), nil)
	jobID := newJobID()
	m.Jobs().Set(jobID, map[string]interface{}{"status": "organizing", "error": nil})

	storage := filepath.Join(t.TempDir(), "PC Game")
	writeFileT(t, filepath.Join(storage, "setup.exe"), []byte("installer"))
	writeFileT(t, filepath.Join(storage, "data", "fg-01.bin"), []byte("payload"))

	m.organizeNZBDownload(jobID, storage, "PC Game", "PC", "", true)

	job, _ := m.Jobs().Get(jobID)
	if detail, _ := job["detail"].(string); !strings.Contains(detail, "GameVault") {
		t.Errorf("detail = %q, want GameVault", detail)
	}

	// Read the archive rather than assert it exists: that is the only assertion
	// separating a real import from one that archived nothing.
	got := tarEntries(t, filepath.Join(cfg.GamesVaultPath, "PC Game.tar"))
	if got["setup.exe"] != "installer" || got["data/fg-01.bin"] != "payload" {
		t.Errorf("archive contents = %v, want both source files", got)
	}
	// A write to the vault reaches the mount's cache, not the remote behind it,
	// so nothing here can tell whether the archive is safe. The download copy
	// stays for a layer that can read the remote to release.
	if !pathExists(filepath.Join(storage, "setup.exe")) {
		t.Error("the download copy was removed on the strength of a local write")
	}
}

// A restart can lose the job update after the archive landed. Re-running
// organize has to finish the job: otherwise every retry reports the same
// failure and the only way out is deleting the archive by hand.
func TestOrganizeGameCompletesWhenTheArchiveIsAlreadyThere(t *testing.T) {
	cfg := newTestConfig(t)
	cfg.VaultArchiveEnabled = true
	jobs := newTestJobs(t)

	content := filepath.Join(t.TempDir(), "Recovered Game")
	writeFileT(t, filepath.Join(content, "setup.exe"), []byte("installer"))
	writeFileT(t, filepath.Join(cfg.GamesVaultPath, "Recovered Game.tar"), []byte("published earlier"))

	qm := newQbitMock(t)
	m := New(cfg, jobs, qm.client())
	jobID := newJobID()
	jobs.Set(jobID, map[string]interface{}{"status": "organizing", "error": nil})

	m.organizeGame(jobID, &qbit.Torrent{Name: "Recovered Game", Hash: "h9", ContentPath: content}, "PC", "", true)

	job, _ := jobs.Get(jobID)
	if status, _ := job["status"].(string); status != "completed" {
		t.Errorf("status = %q, want completed (job=%v)", status, job)
	}
	if !m.Jobs().LibraryHasSourceID("torrent:h9") {
		t.Error("the already-published archive was not tracked in the library")
	}
	for _, call := range qm.deleteCalls() {
		if call.deleteFiles {
			t.Errorf("torrent %s was deleted with its data", call.hash)
		}
	}
}

// Staging may only go once the whole download is in the tar. A failure partway
// through has to leave the download intact.
func TestOrganizeNZBKeepsStagingWhenArchiveFails(t *testing.T) {
	cfg := newTestConfig(t)
	cfg.VaultArchiveEnabled = true
	m := New(cfg, newTestJobs(t), nil)
	jobID := newJobID()
	m.Jobs().Set(jobID, map[string]interface{}{"status": "organizing", "error": nil})

	storage := filepath.Join(t.TempDir(), "PC Game")
	irregularTree(t, storage)

	m.organizeNZBDownload(jobID, storage, "PC Game", "PC", "", true)

	job, _ := m.Jobs().Get(jobID)
	if status, _ := job["status"].(string); status != "error" {
		t.Errorf("status = %q, want error (job=%v)", status, job)
	}
	if !pathExists(filepath.Join(storage, "a-payload.bin")) {
		t.Error("a failed archive removed the staging copy")
	}
	if pathExists(filepath.Join(cfg.GamesVaultPath, "PC Game.tar")) {
		t.Error("a failed archive was published to the vault")
	}
}

// The ROM library needs the raw file: RomM cannot read a tar and GameVault
// never sees that tree, so the option must not reach it.
func TestOrganizeNZBNeverArchivesROMs(t *testing.T) {
	cfg := newTestConfig(t)
	cfg.VaultArchiveEnabled = true
	m := New(cfg, newTestJobs(t), nil)
	jobID := newJobID()
	m.Jobs().Set(jobID, map[string]interface{}{"status": "organizing", "error": nil})

	storage := filepath.Join(t.TempDir(), "Rom Game")
	writeFileT(t, filepath.Join(storage, "rom.sfc"), []byte("x"))

	m.organizeNZBDownload(jobID, storage, "Rom Game", "SNES", "snes", false)

	if !pathExists(filepath.Join(cfg.GamesRomsPath, "snes", "Rom Game", "rom.sfc")) {
		t.Error("ROM not imported as a raw file")
	}
	if pathExists(filepath.Join(cfg.GamesRomsPath, "snes", "Rom Game.tar")) {
		t.Error("a ROM was archived")
	}
}

// A restart can re-enter organize after the archive landed and staging went.
// That is a finished import, not a failure.
func TestOrganizeNZBCompletesFromExistingArchive(t *testing.T) {
	cfg := newTestConfig(t)
	cfg.VaultArchiveEnabled = true
	m := New(cfg, newTestJobs(t), nil)
	jobID := newJobID()
	m.Jobs().Set(jobID, map[string]interface{}{"status": "organizing", "error": nil})

	storage := filepath.Join(t.TempDir(), "Recovered Game")
	dest := filepath.Join(cfg.GamesVaultPath, "Recovered Game.tar")
	writeFileT(t, dest, []byte("tar"))

	m.organizeNZBDownload(jobID, storage, "Recovered Game", "PC", "", true)

	job, _ := m.Jobs().Get(jobID)
	if status, _ := job["status"].(string); status != "completed" {
		t.Errorf("status = %q, want completed (job=%v)", status, job)
	}
	if !m.Jobs().LibraryHasSourceID("nzb:" + dest) {
		t.Error("existing archive was not tracked in the library")
	}
}
