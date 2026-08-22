package download

import (
	"archive/tar"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gamarr/internal/fileops"
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

	// The whole string, not a substring: the archive branch leaves the download
	// in place, so claiming a move is wrong and only the verb says which
	// happened.
	job, _ := m.Jobs().Get(jobID)
	if detail, _ := job["detail"].(string); detail != "Copied to GameVault" {
		t.Errorf("detail = %q, want %q", detail, "Copied to GameVault")
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

// libraryPathFor returns the FilePath the library recorded for a source ID.
func libraryPathFor(t *testing.T, m *Manager, sourceID string) string {
	t.Helper()
	for _, item := range m.Jobs().RecentLibraryItems(50) {
		if item.SourceID == sourceID {
			return item.FilePath
		}
	}
	t.Fatalf("no library row for %q", sourceID)
	return ""
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
	writeFileT(t, filepath.Join(content, "fg-01.bin"), []byte(strings.Repeat("payload", 512)))

	// A real archive of this content, not a stand-in: the accept has to be
	// distinguishable from any file happening to sit at that name.
	staged := filepath.Join(t.TempDir(), "staged.tar")
	if err := fileops.Archive(content, staged); err != nil {
		t.Fatalf("build fixture archive: %v", err)
	}
	published := filepath.Join(cfg.GamesVaultPath, "Recovered Game.tar")
	if err := os.Rename(staged, published); err != nil {
		t.Fatalf("place fixture archive: %v", err)
	}

	qm := newQbitMock(t)
	m := New(cfg, jobs, qm.client())
	jobID := newJobID()
	jobs.Set(jobID, map[string]interface{}{"status": "organizing", "error": nil})

	m.organizeGame(jobID, &qbit.Torrent{Name: "Recovered Game", Hash: "h9", ContentPath: content}, "PC", "", true)

	job, _ := jobs.Get(jobID)
	if status, _ := job["status"].(string); status != "completed" {
		t.Errorf("status = %q, want completed (job=%v)", status, job)
	}
	// The row has to point at something that exists. A path nothing wrote reads
	// as a stored game to whatever releases the download copy.
	recorded := libraryPathFor(t, m, "torrent:h9")
	if _, err := os.Stat(recorded); err != nil {
		t.Errorf("library row FilePath %q does not exist: %v", recorded, err)
	}
	if recorded != published {
		t.Errorf("library row FilePath = %q, want the archive at %q", recorded, published)
	}
	for _, call := range qm.deleteCalls() {
		if call.deleteFiles {
			t.Errorf("torrent %s was deleted with its data", call.hash)
		}
	}
}

// The accepted path has to be the occupant, not the name this import would have
// written, and those two only differ when the vault holds the un-suffixed name.
// An earlier single-file import of the same game lands exactly there, so a
// folder-shaped release arriving later separates them.
func TestOrganizeGameReportsTheOccupantNotTheArchiveName(t *testing.T) {
	cfg := newTestConfig(t)
	cfg.VaultArchiveEnabled = true
	jobs := newTestJobs(t)

	content := filepath.Join(t.TempDir(), "Game")
	writeFileT(t, filepath.Join(content, "setup.exe"), []byte("installer"))

	// A regular file at the un-suffixed name, large enough to pass as this game.
	occupant := filepath.Join(cfg.GamesVaultPath, "Game")
	writeFileT(t, occupant, []byte(strings.Repeat("earlier single-file import", 64)))

	qm := newQbitMock(t)
	m := New(cfg, jobs, qm.client())
	jobID := newJobID()
	jobs.Set(jobID, map[string]interface{}{"status": "organizing", "error": nil})

	m.organizeGame(jobID, &qbit.Torrent{Name: "Game", Hash: "h7", ContentPath: content}, "PC", "", true)

	job, _ := jobs.Get(jobID)
	if status, _ := job["status"].(string); status != "completed" {
		t.Fatalf("status = %q, want completed (job=%v)", status, job)
	}
	recorded := libraryPathFor(t, m, "torrent:h7")
	if recorded != occupant {
		t.Errorf("library row FilePath = %q, want the occupant %q", recorded, occupant)
	}
	if _, err := os.Stat(recorded); err != nil {
		t.Errorf("library row FilePath %q does not exist: %v", recorded, err)
	}
	if pathExists(filepath.Join(cfg.GamesVaultPath, "Game.tar")) {
		t.Error("reported a .tar the import never wrote")
	}
}

// An occupant that cannot be an archive of this content is not this import.
// Accepting one reports a game as stored that was never stored, and the library
// row is what a release decision is read from.
func TestOrganizeGameRefusesAnOccupantThatIsNotTheArchive(t *testing.T) {
	cases := map[string]func(t *testing.T, vault string) string{
		"a file too small to hold the source": func(t *testing.T, vault string) string {
			p := filepath.Join(vault, "Game.tar")
			writeFileT(t, p, []byte("not really an archive"))
			return p
		},
		"a folder from an earlier unarchived import": func(t *testing.T, vault string) string {
			p := filepath.Join(vault, "Game")
			writeFileT(t, filepath.Join(p, "setup.exe"), []byte("old"))
			return p
		},
	}
	for name, seed := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := newTestConfig(t)
			cfg.VaultArchiveEnabled = true
			jobs := newTestJobs(t)
			seed(t, cfg.GamesVaultPath)

			content := filepath.Join(t.TempDir(), "Game")
			writeFileT(t, filepath.Join(content, "setup.exe"), []byte(strings.Repeat("installer", 4096)))

			qm := newQbitMock(t)
			m := New(cfg, jobs, qm.client())
			jobID := newJobID()
			jobs.Set(jobID, map[string]interface{}{"status": "organizing", "error": nil})

			m.organizeGame(jobID, &qbit.Torrent{Name: "Game", Hash: "hx", ContentPath: content}, "PC", "", true)

			job, _ := jobs.Get(jobID)
			if status, _ := job["status"].(string); status != "error" {
				t.Errorf("status = %q, want error (job=%v)", status, job)
			}
			if m.Jobs().LibraryHasSourceID("torrent:hx") {
				t.Error("filed a library row for content that was never stored")
			}
		})
	}
}

// Occupancy has to hold in both flag states. With the flag off the plain import
// path used to write the folder beside an existing archive, storing one game
// twice at full size under a single title.
func TestOrganizeGameDoesNotStoreTwiceWithArchiveOff(t *testing.T) {
	cfg := newTestConfig(t)
	cfg.VaultArchiveEnabled = false
	jobs := newTestJobs(t)

	content := filepath.Join(t.TempDir(), "Game")
	writeFileT(t, filepath.Join(content, "setup.exe"), []byte("installer"))
	writeFileT(t, filepath.Join(cfg.GamesVaultPath, "Game.tar"), []byte(strings.Repeat("archived earlier", 64)))

	qm := newQbitMock(t)
	m := New(cfg, jobs, qm.client())
	jobID := newJobID()
	jobs.Set(jobID, map[string]interface{}{"status": "organizing", "error": nil})

	m.organizeGame(jobID, &qbit.Torrent{Name: "Game", Hash: "h8", ContentPath: content}, "PC", "", true)

	if pathExists(filepath.Join(cfg.GamesVaultPath, "Game")) {
		t.Error("stored the game a second time as a folder beside the archive")
	}
	entries, err := os.ReadDir(cfg.GamesVaultPath)
	if err != nil {
		t.Fatalf("read vault: %v", err)
	}
	var stored []string
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".gamarr.json") {
			stored = append(stored, e.Name())
		}
	}
	if len(stored) != 1 || stored[0] != "Game.tar" {
		t.Errorf("vault holds %v, want only the archive already there", stored)
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

// The usenet path takes the same occupancy decision as the torrent path. Without
// it, fileops.Archive refuses instead, which turns a lost job update into a
// permanent failure rather than a finished import.
func TestOrganizeNZBCompletesWhenTheArchiveIsAlreadyThere(t *testing.T) {
	cfg := newTestConfig(t)
	cfg.VaultArchiveEnabled = true
	m := New(cfg, newTestJobs(t), nil)
	jobID := newJobID()
	m.Jobs().Set(jobID, map[string]interface{}{"status": "organizing", "error": nil})

	storage := filepath.Join(t.TempDir(), "PC Game")
	writeFileT(t, filepath.Join(storage, "setup.exe"), []byte("installer"))
	writeFileT(t, filepath.Join(storage, "fg-01.bin"), []byte(strings.Repeat("payload", 512)))

	staged := filepath.Join(t.TempDir(), "staged.tar")
	if err := fileops.Archive(storage, staged); err != nil {
		t.Fatalf("build fixture archive: %v", err)
	}
	published := filepath.Join(cfg.GamesVaultPath, "PC Game.tar")
	if err := os.Rename(staged, published); err != nil {
		t.Fatalf("place fixture archive: %v", err)
	}

	m.organizeNZBDownload(jobID, storage, "PC Game", "PC", "", true)

	job, _ := m.Jobs().Get(jobID)
	if status, _ := job["status"].(string); status != "completed" {
		t.Errorf("status = %q, want completed (job=%v)", status, job)
	}
	if !m.Jobs().LibraryHasSourceID("nzb:" + published) {
		t.Error("the already-published archive was not tracked in the library")
	}
}

// The reject half of that same decision. An occupant that cannot be an archive
// of this download must not be reported as this download: a completed job plus a
// library row is exactly what a release decision is read from.
func TestOrganizeNZBRefusesAnOccupantThatIsNotTheArchive(t *testing.T) {
	cfg := newTestConfig(t)
	cfg.VaultArchiveEnabled = true
	m := New(cfg, newTestJobs(t), nil)
	jobID := newJobID()
	m.Jobs().Set(jobID, map[string]interface{}{"status": "organizing", "error": nil})

	storage := filepath.Join(t.TempDir(), "PC Game")
	writeFileT(t, filepath.Join(storage, "setup.exe"), []byte(strings.Repeat("installer", 4096)))
	occupant := filepath.Join(cfg.GamesVaultPath, "PC Game.tar")
	writeFileT(t, occupant, []byte("tiny"))

	m.organizeNZBDownload(jobID, storage, "PC Game", "PC", "", true)

	job, _ := m.Jobs().Get(jobID)
	if status, _ := job["status"].(string); status != "error" {
		t.Errorf("status = %q, want error (job=%v)", status, job)
	}
	if m.Jobs().LibraryHasSourceID("nzb:" + occupant) {
		t.Error("filed a library row for content that was never stored")
	}
	if !pathExists(filepath.Join(storage, "setup.exe")) {
		t.Error("the download was not left in place")
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
	// An archive import is a copy on the recovery route too. Staging is gone
	// here, so the name is the only thing the mode can be derived from.
	if detail, _ := job["detail"].(string); detail != "Copied to GameVault" {
		t.Errorf("detail = %q, want %q", detail, "Copied to GameVault")
	}
	if !m.Jobs().LibraryHasSourceID("nzb:" + dest) {
		t.Error("existing archive was not tracked in the library")
	}
}
