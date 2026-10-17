package evolution

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fanice1998/kawa/internal/sandbox"
)

func TestEvolutionLifecycle(t *testing.T) {
	tmpDir := t.TempDir()
	srcDir := filepath.Join(tmpDir, "repo")
	stateDir := filepath.Join(tmpDir, "state")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Initialize a miniature Go module in srcDir
	if err := os.WriteFile(filepath.Join(srcDir, "go.mod"), []byte("module example.com/kawatest\n\ngo 1.24\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "calc.go"), []byte("package kawatest\n\nfunc Add(a, b int) int { return a + b }\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	runner, err := sandbox.NewSeatbeltRunner(true, stateDir, 60*time.Second)
	if err != nil {
		t.Fatal(err)
	}

	engine, err := NewEngine(srcDir, stateDir, runner, nil)
	if err != nil {
		t.Fatal(err)
	}

	// 1. Propose broken code -> must be rejected automatically by sandboxed `go test`
	_, err = engine.Propose(context.Background(), "Broken feature", "Fails compilation", []FilePatch{
		{
			Path:    "broken.go",
			Content: "package kawatest\n\nfunc Broken() { undefinedSymbol() }\n",
		},
	})
	if err == nil {
		t.Fatalf("expected broken code proposal to fail sandbox verification, but succeeded")
	}

	// Verify srcDir was NOT polluted by broken.go
	if _, err := os.Stat(filepath.Join(srcDir, "broken.go")); !os.IsNotExist(err) {
		t.Fatalf("srcDir was polluted by broken.go!")
	}

	// 2. Propose valid new feature + unit test -> must pass sandbox verification and await approval
	prop, err := engine.Propose(context.Background(), "Add Multiply function", "Adds Multiply and unit test", []FilePatch{
		{
			Path:    "multiply.go",
			Content: "package kawatest\n\nfunc Multiply(a, b int) int { return a * b }\n",
		},
		{
			Path:    "multiply_test.go",
			Content: "package kawatest\n\nimport \"testing\"\n\nfunc TestMultiply(t *testing.T) {\n\tif Multiply(3, 4) != 12 { t.Fatal(\"bad\") }\n}\n",
		},
	})
	if err != nil {
		t.Fatalf("expected valid proposal to succeed, got error: %v", err)
	}
	if prop.Status != StatusPendingApproval {
		t.Fatalf("expected status %s, got %s", StatusPendingApproval, prop.Status)
	}
	if !strings.Contains(prop.Diff, "Multiply") {
		t.Fatalf("expected diff to contain Multiply, got: %s", prop.Diff)
	}

	// Before human approval, multiply.go must still NOT exist in live srcDir
	if _, err := os.Stat(filepath.Join(srcDir, "multiply.go")); !os.IsNotExist(err) {
		t.Fatalf("multiply.go should not exist in srcDir before human approval!")
	}

	// 3. Human approves the proposal -> changes are applied to srcDir
	approved, err := engine.Approve(context.Background(), prop.ID)
	if err != nil {
		t.Fatalf("Approve failed: %v", err)
	}
	if approved.Status != StatusApproved {
		t.Fatalf("expected status APPROVED, got %s", approved.Status)
	}

	if _, err := os.Stat(filepath.Join(srcDir, "multiply.go")); err != nil {
		t.Fatalf("expected multiply.go to exist in srcDir after approval: %v", err)
	}
}
