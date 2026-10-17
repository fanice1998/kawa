package sandbox

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestSeatbeltBlocksOutsideDeletionAndModification(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS sandbox-exec test only runs on darwin")
	}

	baseDir := t.TempDir()
	// Create a precious file outside the allowed worktree to simulate user's host files
	preciousFile := filepath.Join(baseDir, "precious_host_file.txt")
	if err := os.WriteFile(preciousFile, []byte("DO_NOT_DELETE_ME"), 0o644); err != nil {
		t.Fatal(err)
	}

	stateDir := filepath.Join(baseDir, "state")
	worktreeDir := filepath.Join(baseDir, "worktree")
	if err := os.MkdirAll(worktreeDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Create a miniature Go module inside worktreeDir whose test tries to delete `preciousFile`!
	goMod := "module example.com/sandboxcheck\n\ngo 1.24\n"
	if err := os.WriteFile(filepath.Join(worktreeDir, "go.mod"), []byte(goMod), 0o644); err != nil {
		t.Fatal(err)
	}

	testCode := fmt.Sprintf(`package sandboxcheck

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAttemptDeleteHostFile(t *testing.T) {
	// 1. Writing inside the worktree MUST succeed
	if err := os.WriteFile(filepath.Join(".", "inside_allowed.txt"), []byte("ok"), 0o644); err != nil {
		t.Fatalf("expected write inside worktree to succeed, got: %%v", err)
	}

	// 2. Attempting to delete precious host file outside worktree MUST be blocked by macOS kernel!
	err := os.Remove(%q)
	if err == nil {
		t.Fatalf("SECURITY BREACH: os.Remove outside worktree succeeded!")
	}
	t.Logf("Kernel successfully blocked os.Remove outside worktree: %%v", err)
}
`, preciousFile)

	if err := os.WriteFile(filepath.Join(worktreeDir, "escape_test.go"), []byte(testCode), 0o644); err != nil {
		t.Fatal(err)
	}

	runner, err := NewSeatbeltRunner(true, stateDir, 60*time.Second)
	if err != nil {
		t.Fatalf("NewSeatbeltRunner failed: %v", err)
	}

	res, err := runner.RunGo(context.Background(), worktreeDir, "test", "-v", "./...")
	if err != nil {
		t.Fatalf("Sandboxed go test failed unexpectedly (exit=%d):\nstdout: %s\nstderr: %s\nerr: %v",
			res.ExitCode, res.Stdout, res.Stderr, err)
	}

	// Verify preciousFile still exists and has its original content
	data, err := os.ReadFile(preciousFile)
	if err != nil || string(data) != "DO_NOT_DELETE_ME" {
		t.Fatalf("precious host file was damaged! err=%v, content=%q", err, string(data))
	}
}
