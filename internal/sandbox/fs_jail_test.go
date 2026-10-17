package sandbox

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestFSJailBlocksTraversalAndSymlinkEscape(t *testing.T) {
	tmpOutside := t.TempDir()
	secretFile := filepath.Join(tmpOutside, "secret.txt")
	if err := os.WriteFile(secretFile, []byte("top-secret-system-file"), 0o644); err != nil {
		t.Fatal(err)
	}

	jailRoot := filepath.Join(tmpOutside, "workspace")
	backupDir := filepath.Join(tmpOutside, "backups")
	jail, err := NewFSJail(jailRoot, backupDir, false)
	if err != nil {
		t.Fatalf("NewFSJail failed: %v", err)
	}

	// 1. Normal write & read inside jail should succeed
	if err := jail.WriteFile("notes/hello.txt", []byte("v1")); err != nil {
		t.Fatalf("WriteFile inside jail failed: %v", err)
	}
	if err := jail.WriteFile("notes/hello.txt", []byte("v2")); err != nil {
		t.Fatalf("WriteFile overwrite inside jail failed: %v", err)
	}
	got, err := jail.ReadFile("notes/hello.txt")
	if err != nil || string(got) != "v2" {
		t.Fatalf("ReadFile got %q, err %v; want v2", string(got), err)
	}

	// Verify backup was created on overwrite
	baks, err := os.ReadDir(backupDir)
	if err != nil || len(baks) == 0 {
		t.Fatalf("expected backup file in %s, found %d (err: %v)", backupDir, len(baks), err)
	}

	// 2. Relative path traversal (../secret.txt) must be blocked
	if _, err := jail.ReadFile("../secret.txt"); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("expected ErrAccessDenied on ../secret.txt, got: %v", err)
	}
	if err := jail.WriteFile("../hacked.txt", []byte("bad")); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("expected ErrAccessDenied on write ../hacked.txt, got: %v", err)
	}

	// 3. Absolute path must be blocked
	if _, err := jail.ReadFile(secretFile); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("expected ErrAccessDenied on absolute path %s, got: %v", secretFile, err)
	}

	// 4. Symlink inside jail pointing outside jail must be blocked by os.Root kernel check
	symlinkPath := filepath.Join(jailRoot, "escape_link.txt")
	if err := os.Symlink(secretFile, symlinkPath); err != nil {
		t.Fatalf("create symlink: %v", err)
	}
	if _, err := jail.ReadFile("escape_link.txt"); err == nil {
		t.Fatalf("expected os.Root to block reading symlink pointing outside jail, but succeeded!")
	}
	if err := jail.WriteFile("escape_link.txt", []byte("overwrite-outside")); err == nil {
		t.Fatalf("expected os.Root to block writing symlink pointing outside jail, but succeeded!")
	}

	// Verify outside secret file is completely untouched
	outsideBytes, err := os.ReadFile(secretFile)
	if err != nil || string(outsideBytes) != "top-secret-system-file" {
		t.Fatalf("outside secret file was modified or deleted! content=%q err=%v", string(outsideBytes), err)
	}

	// 5. Protected paths (like config.toml or .git/config) must be blocked from writing
	if err := jail.WriteFile("config.toml", []byte("malicious")); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("expected ErrAccessDenied when writing protected config.toml, got: %v", err)
	}
}
