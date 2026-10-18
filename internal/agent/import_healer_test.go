package agent

import (
	"strings"
	"testing"
)

func TestAutoHealGoImports(t *testing.T) {
	src := `package tools

type RichMessageTool struct{}

func (r *RichMessageTool) Execute(ctx context.Context, args string) error {
	var payload struct{}
	if err := json.Unmarshal([]byte(args), &payload); err != nil {
		return fmt.Errorf("fail: %w", err)
	}
	return nil
}
`

	healed, changed := AutoHealGoImports(src)
	if !changed {
		t.Fatalf("expected imports to be healed, but changed=false")
	}

	if !strings.Contains(healed, `"context"`) {
		t.Errorf("expected context import in healed code, got:\n%s", healed)
	}
	if !strings.Contains(healed, `"encoding/json"`) {
		t.Errorf("expected encoding/json import in healed code, got:\n%s", healed)
	}
	if !strings.Contains(healed, `"fmt"`) {
		t.Errorf("expected fmt import in healed code, got:\n%s", healed)
	}
}

func TestCleanUnusedImports(t *testing.T) {
	src := `package tools

import (
	"context"
	"fmt"
)

func Do(ctx context.Context) {}
`

	errStr := "internal/tools/tool.go:5:2: \"fmt\" imported and not used"
	cleaned, ok := CleanUnusedImports(src, errStr)
	if !ok {
		t.Fatalf("expected CleanUnusedImports to succeed")
	}

	if strings.Contains(cleaned, `"fmt"`) {
		t.Errorf("expected fmt to be removed, got:\n%s", cleaned)
	}
	if !strings.Contains(cleaned, `"context"`) {
		t.Errorf("expected context to remain, got:\n%s", cleaned)
	}
}

func TestHealImportsFromCompilerError(t *testing.T) {
	workingFiles := map[string]string{
		"internal/tools/custom_tools.go": `package tools

import (
	"github.com/fanice1998/kawa/internal/channel"
)

func Run() {
	var m map[string]any
	_ = json.Unmarshal(nil, &m)
}
`,
	}

	compilerErr := `$ go vet ./...
# github.com/fanice1998/kawa/internal/tools
internal/tools/custom_tools.go:9:6: undefined: json`

	changed := HealImportsFromCompilerError(workingFiles, compilerErr)
	if !changed {
		t.Fatalf("expected HealImportsFromCompilerError to return true")
	}

	res := workingFiles["internal/tools/custom_tools.go"]
	if !strings.Contains(res, `"encoding/json"`) {
		t.Errorf("expected encoding/json in workingFiles, got:\n%s", res)
	}
}
