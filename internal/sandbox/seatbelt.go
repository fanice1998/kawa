package sandbox

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// SeatbeltRunner executes whitelisted development commands (such as `go vet`, `go test`, `go build`)
// inside a macOS kernel-level `sandbox-exec` Seatbelt profile.
// The profile enforces `(deny file-write*)` globally across the host machine and ONLY permits
// writes inside the specified workDir and sandboxTmpDir.
type SeatbeltRunner struct {
	useSeatbelt   bool
	sandboxTmpDir string
	timeout       time.Duration
}

// CommandResult captures stdout, stderr, exit code, and duration of a sandboxed execution.
type CommandResult struct {
	Command  string
	Stdout   string
	Stderr   string
	ExitCode int
	Duration time.Duration
}

// NewSeatbeltRunner creates a new runner.
func NewSeatbeltRunner(useSeatbelt bool, stateDir string, timeout time.Duration) (*SeatbeltRunner, error) {
	absState, err := filepath.Abs(stateDir)
	if err != nil {
		return nil, fmt.Errorf("resolve state dir: %w", err)
	}
	// Resolve symlinks (important on macOS where /var -> /private/var and /tmp -> /private/tmp)
	if err := os.MkdirAll(absState, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir state dir: %w", err)
	}
	realState, err := filepath.EvalSymlinks(absState)
	if err == nil {
		absState = realState
	}

	tmpDir := filepath.Join(absState, "tmp")
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir sandbox tmp dir: %w", err)
	}
	if timeout <= 0 {
		timeout = 120 * time.Second
	}

	return &SeatbeltRunner{
		useSeatbelt:   useSeatbelt && runtime.GOOS == "darwin",
		sandboxTmpDir: tmpDir,
		timeout:       timeout,
	}, nil
}

// BuildSeatbeltProfile generates the Apple Seatbelt (.sb) configuration string.
// It allows reading files/libraries needed to compile Go, blocks all file writes/deletes across
// the entire computer (`(deny file-write*)`), and explicitly allows writes ONLY inside
// allowedWorkDir and sandboxTmpDir.
func (r *SeatbeltRunner) BuildSeatbeltProfile(allowedWorkDir string) (string, error) {
	absWork, err := filepath.Abs(allowedWorkDir)
	if err != nil {
		return "", fmt.Errorf("abs workdir: %w", err)
	}
	if realWork, err := filepath.EvalSymlinks(absWork); err == nil {
		absWork = realWork
	}
	absTmp := r.sandboxTmpDir
	if realTmp, err := filepath.EvalSymlinks(absTmp); err == nil {
		absTmp = realTmp
	}

	profile := fmt.Sprintf(`(version 1)
(allow default)
;; Block all file modifications, writes, moves, chmods, and deletions across the entire system
(deny file-write*)
;; Allow file writes ONLY inside the isolated worktree and sandbox temp directory
(allow file-write*
    (subpath %q)
    (subpath %q)
    (literal "/dev/null")
    (literal "/dev/zero")
    (literal "/dev/random")
    (literal "/dev/urandom")
    (literal "/dev/dtracehelper"))
`, absWork, absTmp)

	return profile, nil
}

// RunGo executes a whitelisted `go` subcommand (`vet`, `test`, `build`, `mod`) inside `workDir`
// under the macOS Seatbelt sandbox.
func (r *SeatbeltRunner) RunGo(ctx context.Context, workDir string, goArgs ...string) (*CommandResult, error) {
	if len(goArgs) == 0 {
		return nil, fmt.Errorf("%w: empty go arguments", ErrAccessDenied)
	}
	sub := goArgs[0]
	switch sub {
	case "vet", "test", "build", "mod", "env", "version":
		// Whitelisted safe go subcommands
	default:
		return nil, fmt.Errorf("%w: go subcommand %q is not whitelisted", ErrAccessDenied, sub)
	}

	return r.runWhitelisted(ctx, workDir, "go", goArgs...)
}

func (r *SeatbeltRunner) runWhitelisted(ctx context.Context, workDir string, bin string, args ...string) (*CommandResult, error) {
	absWork, err := filepath.Abs(workDir)
	if err != nil {
		return nil, fmt.Errorf("resolve workdir: %w", err)
	}
	if realWork, err := filepath.EvalSymlinks(absWork); err == nil {
		absWork = realWork
	}

	goCacheDir := filepath.Join(r.sandboxTmpDir, "gocache")
	goTmpDir := filepath.Join(r.sandboxTmpDir, "gotmp")
	_ = os.MkdirAll(goCacheDir, 0o755)
	_ = os.MkdirAll(goTmpDir, 0o755)

	runCtx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	var cmd *exec.Cmd
	if r.useSeatbelt {
		profile, err := r.BuildSeatbeltProfile(absWork)
		if err != nil {
			return nil, err
		}
		seatbeltArgs := make([]string, 0, len(args)+3)
		seatbeltArgs = append(seatbeltArgs, "-p", profile, bin)
		seatbeltArgs = append(seatbeltArgs, args...)
		cmd = exec.CommandContext(runCtx, "/usr/bin/sandbox-exec", seatbeltArgs...)
	} else {
		cmd = exec.CommandContext(runCtx, bin, args...)
	}

	cmd.Dir = absWork

	// Override TMPDIR and GOCACHE so Go writes temporary artifacts strictly inside sandboxTmpDir
	env := os.Environ()
	filteredEnv := make([]string, 0, len(env)+3)
	for _, kv := range env {
		if strings.HasPrefix(kv, "TMPDIR=") || strings.HasPrefix(kv, "GOCACHE=") || strings.HasPrefix(kv, "GOTMPDIR=") {
			continue
		}
		filteredEnv = append(filteredEnv, kv)
	}
	filteredEnv = append(filteredEnv,
		"TMPDIR="+goTmpDir,
		"GOTMPDIR="+goTmpDir,
		"GOCACHE="+goCacheDir,
	)
	cmd.Env = filteredEnv

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	start := time.Now()
	runErr := cmd.Run()
	duration := time.Since(start)

	exitCode := 0
	if runErr != nil {
		if exitErr, ok := runErr.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = -1
		}
	}

	return &CommandResult{
		Command:  fmt.Sprintf("%s %s", bin, strings.Join(args, " ")),
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		ExitCode: exitCode,
		Duration: duration,
	}, runErr
}
