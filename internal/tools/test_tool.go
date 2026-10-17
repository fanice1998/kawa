package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/fanice1998/kawa/internal/sandbox"
)

// SelfTestTool runs `go vet` and `go test` on Kawa's codebase inside the macOS Seatbelt sandbox.
type SelfTestTool struct {
	sourceDir string
	runner    *sandbox.SeatbeltRunner
}

func NewSelfTestTool(sourceDir string, runner *sandbox.SeatbeltRunner) *SelfTestTool {
	return &SelfTestTool{
		sourceDir: sourceDir,
		runner:    runner,
	}
}

func (t *SelfTestTool) Name() string { return "run_self_test" }
func (t *SelfTestTool) Description() string {
	return "Run `go vet` and `go test` on Kawa's codebase inside the macOS Seatbelt kernel sandbox to verify code health."
}
func (t *SelfTestTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"package": {
				"type": "string",
				"description": "Go package pattern to test (default './...')."
			}
		}
	}`)
}

func (t *SelfTestTool) Execute(ctx context.Context, rawArgs string) (string, error) {
	var args struct {
		Package string `json:"package"`
	}
	_ = json.Unmarshal([]byte(rawArgs), &args)
	pkg := strings.TrimSpace(args.Package)
	if pkg == "" {
		pkg = "./..."
	}

	var sb strings.Builder
	vetRes, vetErr := t.runner.RunGo(ctx, t.sourceDir, "vet", pkg)
	sb.WriteString(fmt.Sprintf("=== go vet %s (exit=%d, %s) ===\n", pkg, vetRes.ExitCode, vetRes.Duration.Round(1e6)))
	if vetRes.Stdout != "" {
		sb.WriteString(vetRes.Stdout + "\n")
	}
	if vetRes.Stderr != "" {
		sb.WriteString(vetRes.Stderr + "\n")
	}
	if vetErr != nil {
		return sb.String(), nil
	}

	testRes, _ := t.runner.RunGo(ctx, t.sourceDir, "test", "-v", pkg)
	sb.WriteString(fmt.Sprintf("\n=== go test -v %s (exit=%d, %s) ===\n", pkg, testRes.ExitCode, testRes.Duration.Round(1e6)))
	if testRes.Stdout != "" {
		sb.WriteString(testRes.Stdout + "\n")
	}
	if testRes.Stderr != "" {
		sb.WriteString(testRes.Stderr + "\n")
	}
	return sb.String(), nil
}
