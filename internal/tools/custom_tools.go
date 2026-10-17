package tools

import (
	"github.com/fanice1998/kawa/internal/config"
	"github.com/fanice1998/kawa/internal/memory"
	"github.com/fanice1998/kawa/internal/sandbox"
	"github.com/fanice1998/kawa/internal/scheduler"
)

// CustomToolDeps bundles dependencies available to self-developed tools.
type CustomToolDeps struct {
	Config         *config.Config
	WorkspaceJail  *sandbox.FSJail
	SourceJail     *sandbox.FSJail
	SeatbeltRunner *sandbox.SeatbeltRunner
	Memory         *memory.Engine
	Scheduler      *scheduler.Scheduler
}

// RegisterCustomTools is the dedicated extension hook for Kawa's self-developed tools.
// When Kawa develops a new tool for itself via `propose_self_evolution`, it should:
// 1. Create `internal/tools/<feature>_tool.go` (and `internal/tools/<feature>_tool_test.go`)
// 2. Register the new tool inside `RegisterCustomTools` below.
func RegisterCustomTools(reg *Registry, deps CustomToolDeps) {
	_ = reg
	_ = deps
	// Self-evolved custom tools are registered here.
}
