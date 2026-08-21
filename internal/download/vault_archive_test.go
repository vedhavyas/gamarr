package download

import (
	"path/filepath"
	"strings"
	"testing"

	"gamarr/internal/qbit"
)

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
	if !pathExists(dest) {
		t.Fatalf("no archive at %s", dest)
	}
	if pathExists(filepath.Join(cfg.GamesVaultPath, "007 First Light (2025)")) {
		t.Error("the folder was written to the vault as well as the archive")
	}
	if !m.Jobs().LibraryHasSourceID("torrent:h1") {
		t.Error("archive was not tracked in the library")
	}

	// An archive is a copy, so the seeded data must survive and the torrent
	// must not be deleted with its files.
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
	writeFileT(t, filepath.Join(storage, "setup.exe"), []byte("x"))

	m.organizeNZBDownload(jobID, storage, "PC Game", "PC", "", true)

	job, _ := m.Jobs().Get(jobID)
	if detail, _ := job["detail"].(string); !strings.Contains(detail, "GameVault") {
		t.Errorf("detail = %q, want GameVault", detail)
	}
	if !pathExists(filepath.Join(cfg.GamesVaultPath, "PC Game.tar")) {
		t.Error("usenet content not archived into the vault")
	}
	// The move this replaced consumed staging, and a 30-70 GB repack left
	// behind would fill the disk.
	if pathExists(storage) {
		t.Error("usenet staging survived the archive")
	}
}

// Staging may only go once the tar is on disk under its final name. A failed
// archive has to leave the download intact.
func TestOrganizeNZBKeepsStagingWhenArchiveFails(t *testing.T) {
	cfg := newTestConfig(t)
	cfg.VaultArchiveEnabled = true
	// A file where the vault directory belongs, so creating it fails.
	blocked := filepath.Join(t.TempDir(), "blocked")
	writeFileT(t, blocked, []byte("not a dir"))
	cfg.GamesVaultPath = filepath.Join(blocked, "vault")

	m := New(cfg, newTestJobs(t), nil)
	jobID := newJobID()
	m.Jobs().Set(jobID, map[string]interface{}{"status": "organizing", "error": nil})

	storage := filepath.Join(t.TempDir(), "PC Game")
	writeFileT(t, filepath.Join(storage, "setup.exe"), []byte("x"))

	m.organizeNZBDownload(jobID, storage, "PC Game", "PC", "", true)

	job, _ := m.Jobs().Get(jobID)
	if status, _ := job["status"].(string); status != "error" {
		t.Errorf("status = %q, want error (job=%v)", status, job)
	}
	if !pathExists(filepath.Join(storage, "setup.exe")) {
		t.Error("a failed archive removed the staging copy")
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

// Archiving leaves the staging copy in place, so a restart can re-enter
// organize with the archive already written. That is a finished import, not a
// failure.
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
