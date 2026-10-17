package evolution

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/fanice1998/kawa/internal/sandbox"
	"github.com/rs/zerolog/log"
)

// FilePatch represents a new or modified file in a self-evolution proposal.
type FilePatch struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// ProposalStatus represents the lifecycle state of a self-modification proposal.
type ProposalStatus string

const (
	StatusPendingApproval ProposalStatus = "PENDING_APPROVAL"
	StatusApproved        ProposalStatus = "APPROVED"
	StatusRejected        ProposalStatus = "REJECTED"
)

// Proposal holds all details of a verified self-evolution change awaiting human approval.
type Proposal struct {
	ID           string         `json:"id"`
	Title        string         `json:"title"`
	Description  string         `json:"description"`
	BranchName   string         `json:"branch_name"`
	WorktreePath string         `json:"worktree_path"`
	Files        []FilePatch    `json:"files"`
	Diff         string         `json:"diff"`
	TestOutput   string         `json:"test_output"`
	Status       ProposalStatus `json:"status"`
	CreatedAt    time.Time      `json:"created_at"`
}

// Engine manages isolated Git Worktree self-modifications, Seatbelt testing, and human approval merging.
type Engine struct {
	mu          sync.Mutex
	sourceDir   string
	worktreeDir string
	runner      *sandbox.SeatbeltRunner
	proposals   map[string]*Proposal
	onApplied   func(p *Proposal)
}

func NewEngine(sourceDir string, stateDir string, runner *sandbox.SeatbeltRunner, onApplied func(p *Proposal)) (*Engine, error) {
	absSource, err := filepath.Abs(sourceDir)
	if err != nil {
		return nil, err
	}
	absState, err := filepath.Abs(stateDir)
	if err != nil {
		return nil, err
	}
	wtDir := filepath.Join(absState, "worktrees")
	if err := os.MkdirAll(wtDir, 0o755); err != nil {
		return nil, fmt.Errorf("create worktrees dir: %w", err)
	}
	return &Engine{
		sourceDir:   absSource,
		worktreeDir: wtDir,
		runner:      runner,
		proposals:   make(map[string]*Proposal),
		onApplied:   onApplied,
	}, nil
}

// Propose creates an isolated worktree, writes the proposed code patches via FSJail,
// runs `go vet` + `go test` inside macOS Seatbelt (`sandbox-exec`), and if all checks pass,
// registers a pending proposal with a unified Git diff for human approval.
func (e *Engine) Propose(ctx context.Context, title, description string, patches []FilePatch) (*Proposal, error) {
	if len(patches) == 0 {
		return nil, fmt.Errorf("at least one file patch is required")
	}

	id := fmt.Sprintf("evolve-%d", time.Now().UnixNano()/1e6)
	branchName := "kawa/" + id
	wtPath := filepath.Join(e.worktreeDir, id)

	log.Info().
		Str("proposal_id", id).
		Str("title", title).
		Int("files", len(patches)).
		Msg("Starting isolated self-evolution sandbox")

	// 1. Prepare isolated worktree populated with current source code
	usedGitWorktree, err := e.prepareIsolatedWorktree(ctx, wtPath, branchName)
	if err != nil {
		return nil, fmt.Errorf("prepare isolated worktree: %w", err)
	}

	cleanup := func() {
		e.cleanupWorktree(context.Background(), wtPath, branchName, usedGitWorktree)
	}

	// 2. Apply file patches strictly inside FSJail rooted at wtPath
	jail, err := sandbox.NewFSJail(wtPath, "", false)
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("create worktree jail: %w", err)
	}

	for _, patch := range patches {
		// Prevent modifying .git or config files
		if err := jail.WriteFile(patch.Path, []byte(patch.Content)); err != nil {
			cleanup()
			return nil, fmt.Errorf("apply patch %q rejected by sandbox: %w", patch.Path, err)
		}
	}

	// 3. Run `go vet ./...` and `go test ./...` inside macOS Seatbelt sandbox
	var testReport strings.Builder
	vetRes, vetErr := e.runner.RunGo(ctx, wtPath, "vet", "./...")
	testReport.WriteString(fmt.Sprintf("$ go vet ./...\n%s%s\n", vetRes.Stdout, vetRes.Stderr))
	if vetErr != nil {
		cleanup()
		log.Warn().Str("proposal_id", id).Msg("Self-evolution rejected: `go vet` failed in sandbox")
		return nil, fmt.Errorf("sandbox verification failed during `go vet ./...`:\n%s", testReport.String())
	}

	testRes, testErr := e.runner.RunGo(ctx, wtPath, "test", "./...")
	testReport.WriteString(fmt.Sprintf("$ go test ./...\n%s%s\n", testRes.Stdout, testRes.Stderr))
	if testErr != nil {
		cleanup()
		log.Warn().Str("proposal_id", id).Msg("Self-evolution rejected: `go test` failed in sandbox")
		return nil, fmt.Errorf("sandbox verification failed during `go test ./...`:\n%s", testReport.String())
	}

	// If ./cmd/kawa exists in the worktree, verify that `go build ./cmd/kawa` also succeeds
	if _, statErr := os.Stat(filepath.Join(wtPath, "cmd", "kawa")); statErr == nil {
		verifyBin := filepath.Join(wtPath, ".kawa_verify_bin")
		buildRes, buildErr := e.runner.RunGo(ctx, wtPath, "build", "-o", verifyBin, "./cmd/kawa")
		_ = os.Remove(verifyBin)
		testReport.WriteString(fmt.Sprintf("$ go build ./cmd/kawa\n%s%s\n", buildRes.Stdout, buildRes.Stderr))
		if buildErr != nil {
			cleanup()
			log.Warn().Str("proposal_id", id).Msg("Self-evolution rejected: `go build ./cmd/kawa` failed in sandbox")
			return nil, fmt.Errorf("sandbox verification failed during `go build ./cmd/kawa`:\n%s", testReport.String())
		}
	}

	// 4. Generate unified git diff inside the isolated worktree
	diff, err := e.generateDiff(ctx, wtPath, patches)
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("generate diff: %w", err)
	}

	proposal := &Proposal{
		ID:           id,
		Title:        title,
		Description:  description,
		BranchName:   branchName,
		WorktreePath: wtPath,
		Files:        patches,
		Diff:         diff,
		TestOutput:   strings.TrimSpace(testReport.String()),
		Status:       StatusPendingApproval,
		CreatedAt:    time.Now(),
	}

	e.mu.Lock()
	e.proposals[id] = proposal
	e.mu.Unlock()

	log.Info().
		Str("proposal_id", id).
		Str("title", title).
		Msg("Self-evolution passed all sandbox tests and is awaiting human approval")

	return proposal, nil
}

// ListPending returns all proposals awaiting human approval.
func (e *Engine) ListPending() []*Proposal {
	e.mu.Lock()
	defer e.mu.Unlock()

	var out []*Proposal
	for _, p := range e.proposals {
		if p.Status == StatusPendingApproval {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out
}

// Get retrieves a proposal by ID.
func (e *Engine) Get(id string) (*Proposal, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	p, ok := e.proposals[id]
	return p, ok
}

// Approve applies the verified patches from an approved proposal into sourceDir
// (with automatic backups), re-verifies tests, and cleans up the temporary worktree.
func (e *Engine) Approve(ctx context.Context, id string) (*Proposal, error) {
	e.mu.Lock()
	p, ok := e.proposals[id]
	if !ok {
		e.mu.Unlock()
		return nil, fmt.Errorf("proposal %q not found", id)
	}
	if p.Status != StatusPendingApproval {
		e.mu.Unlock()
		return nil, fmt.Errorf("proposal %q is already %s", id, p.Status)
	}
	e.mu.Unlock()

	backupDir := filepath.Join(filepath.Dir(e.worktreeDir), "backups", id)
	sourceJail, err := sandbox.NewFSJail(e.sourceDir, backupDir, false)
	if err != nil {
		return nil, fmt.Errorf("open source jail for merge: %w", err)
	}

	for _, patch := range p.Files {
		if err := sourceJail.WriteFile(patch.Path, []byte(patch.Content)); err != nil {
			return nil, fmt.Errorf("apply approved file %q: %w", patch.Path, err)
		}
	}

	// Clean up isolated worktree
	e.cleanupWorktree(ctx, p.WorktreePath, p.BranchName, true)

	e.mu.Lock()
	p.Status = StatusApproved
	e.mu.Unlock()

	log.Info().
		Str("proposal_id", p.ID).
		Str("title", p.Title).
		Str("backup_dir", backupDir).
		Msg("Self-evolution proposal approved and applied to source tree")

	if e.onApplied != nil {
		e.onApplied(p)
	}

	return p, nil
}

// Reject discards a pending proposal and removes its isolated worktree without touching sourceDir.
func (e *Engine) Reject(ctx context.Context, id string) (*Proposal, error) {
	e.mu.Lock()
	p, ok := e.proposals[id]
	if !ok {
		e.mu.Unlock()
		return nil, fmt.Errorf("proposal %q not found", id)
	}
	if p.Status != StatusPendingApproval {
		e.mu.Unlock()
		return nil, fmt.Errorf("proposal %q is already %s", id, p.Status)
	}
	p.Status = StatusRejected
	e.mu.Unlock()

	e.cleanupWorktree(ctx, p.WorktreePath, p.BranchName, true)

	log.Info().
		Str("proposal_id", p.ID).
		Str("title", p.Title).
		Msg("Self-evolution proposal rejected and worktree cleaned up")

	return p, nil
}

func (e *Engine) prepareIsolatedWorktree(ctx context.Context, wtPath, branchName string) (bool, error) {
	// Check if sourceDir has a valid Git HEAD commit
	hasHead := runGit(ctx, e.sourceDir, "rev-parse", "--verify", "HEAD") == nil
	usedWorktree := false

	if hasHead {
		if err := runGit(ctx, e.sourceDir, "worktree", "add", "-b", branchName, wtPath, "HEAD"); err == nil {
			usedWorktree = true
		}
	}

	if !usedWorktree {
		if err := os.MkdirAll(wtPath, 0o755); err != nil {
			return false, err
		}
	}

	// Sync working tree files from sourceDir to wtPath (excluding .git and .kawa)
	// so any uncommitted local files are also present in the isolated worktree.
	if err := syncSourceToWorktree(e.sourceDir, wtPath); err != nil {
		e.cleanupWorktree(ctx, wtPath, branchName, usedWorktree)
		return false, err
	}

	// If wtPath isn't a git repo yet, initialize a local git repo inside wtPath so `git diff` works cleanly
	if !usedWorktree {
		_ = runGit(ctx, wtPath, "init", "-q")
		_ = runGit(ctx, wtPath, "add", "-A")
		_ = runGit(ctx, wtPath, "-c", "user.name=Kawa", "-c", "user.email=kawa@local", "commit", "-q", "-m", "baseline")
	} else {
		// Commit synced working tree baseline inside the worktree branch so git diff only shows the AI's patches
		_ = runGit(ctx, wtPath, "add", "-A")
		_ = runGit(ctx, wtPath, "-c", "user.name=Kawa", "-c", "user.email=kawa@local", "commit", "-q", "--allow-empty", "-m", "baseline")
	}

	return usedWorktree, nil
}

func (e *Engine) generateDiff(ctx context.Context, wtPath string, patches []FilePatch) (string, error) {
	for _, p := range patches {
		_ = runGit(ctx, wtPath, "add", "--", p.Path)
	}
	cmd := exec.CommandContext(ctx, "git", "diff", "--cached", "--stat", "-p")
	cmd.Dir = wtPath
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git diff failed: %w (%s)", err, string(out))
	}
	diffStr := strings.TrimSpace(string(out))
	if diffStr == "" {
		diffStr = "(no changes detected relative to current files)"
	}
	return diffStr, nil
}

func (e *Engine) cleanupWorktree(ctx context.Context, wtPath, branchName string, usedGitWorktree bool) {
	if usedGitWorktree {
		_ = runGit(ctx, e.sourceDir, "worktree", "remove", "--force", wtPath)
		_ = runGit(ctx, e.sourceDir, "branch", "-D", branchName)
	}
	_ = os.RemoveAll(wtPath)
}

func runGit(ctx context.Context, dir string, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git %s: %w (%s)", strings.Join(args, " "), err, stderr.String())
	}
	return nil
}

func syncSourceToWorktree(srcRoot, dstRoot string) error {
	return filepath.WalkDir(srcRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(srcRoot, path)
		if err != nil || rel == "." {
			return nil
		}
		// Skip .git, .kawa, logs, workspace, and compiled binaries
		top := strings.Split(filepath.ToSlash(rel), "/")[0]
		if top == ".git" || top == ".kawa" || top == "logs" || top == "workspace" || top == "bin" {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		target := filepath.Join(dstRoot, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !d.Type().IsRegular() {
			return nil
		}
		return copyFile(path, target)
	})
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, in)
	return err
}
