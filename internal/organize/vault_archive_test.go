package organize

import (
	"os"
	"path/filepath"
	"testing"

	"gamarr/internal/config"
)

func newArchivePipeline(t *testing.T) (*Pipeline, string) {
	t.Helper()
	root := t.TempDir()
	vault := filepath.Join(root, "vault")
	cfg := &config.Config{
		GamesVaultPath:      vault,
		GamesRomsPath:       filepath.Join(root, "roms"),
		VaultArchiveEnabled: true,
	}
	return NewPipeline(cfg), vault
}

func TestOrganizePCArchivesDirectory(t *testing.T) {
	p, vault := newArchivePipeline(t)
	src := filepath.Join(t.TempDir(), "Some.Game.v1.0-FitGirl")
	writeFile(t, filepath.Join(src, "setup.exe"), "SETUP")
	writeFile(t, filepath.Join(src, "fg-01.bin"), "PAYLOAD")

	dest, err := p.OrganizeGame(src, "PC", "", true)
	if err != nil {
		t.Fatalf("OrganizeGame: %v", err)
	}

	if filepath.Dir(dest) != vault || filepath.Ext(dest) != ".tar" {
		t.Errorf("dest = %q, want a .tar in %q", dest, vault)
	}
	if _, err := os.Stat(dest); err != nil {
		t.Fatalf("stat archive: %v", err)
	}
	if _, err := os.Stat(filepath.Join(src, "fg-01.bin")); err != nil {
		t.Error("archiving destroyed the source")
	}
}

// A single file is already the one-file-per-game unit GameVault indexes, so
// wrapping it would only rewrite the bytes for nothing.
func TestOrganizePCLeavesSingleFileAlone(t *testing.T) {
	p, vault := newArchivePipeline(t)
	src := filepath.Join(t.TempDir(), "Lone Game.exe")
	writeFile(t, src, "SETUP")

	dest, err := p.OrganizeGame(src, "PC", "", true)
	if err != nil {
		t.Fatalf("OrganizeGame: %v", err)
	}

	if dest != filepath.Join(vault, "Lone Game.exe") {
		t.Errorf("dest = %q, want the file imported unwrapped", dest)
	}
}
