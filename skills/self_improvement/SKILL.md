---
name: self_improvement
description: Step-by-step guide for Kawa to safely inspect its own Go source code, develop new tools/features in internal/tools/custom_tools.go, verify in the Seatbelt sandbox, and propose self-evolution.
---

# Kawa Self-Improvement & Autonomous Feature Development Skill

When the user asks you to develop a new feature, create a new tool, optimize existing code, or fix a bug in yourself, follow this strict workflow:

## Step 1: Inspect Current Source Code
1. Call `read_file` with `{"scope": "source", "path": "internal/tools/custom_tools.go"}` and `{"scope": "source", "path": "internal/tools/tool.go"}`.
2. Notice that `RegisterCustomTools(reg *Registry, deps CustomToolDeps)` in `internal/tools/custom_tools.go` is your dedicated plug-and-play registration point! You do NOT need to modify `cmd/kawa/main.go`.

## Step 2: Implement the New Tool & Unit Test
1. Create a new file `internal/tools/<feature>_tool.go` implementing the `Tool` interface:
   - `Name() string`
   - `Description() string`
   - `Parameters() json.RawMessage`
   - `Execute(ctx context.Context, rawArgs string) (string, error)`
2. Create a corresponding unit test `internal/tools/<feature>_tool_test.go` to verify your new tool works properly.
3. Update `internal/tools/custom_tools.go` to register your new tool inside `RegisterCustomTools`:
   ```go
   func RegisterCustomTools(reg *Registry, deps CustomToolDeps) {
       reg.Register(NewMyCustomTool(deps))
   }
   ```

## Step 3: Submit via `propose_self_evolution`
Call `propose_self_evolution` with:
- `title`: Concise summary of the new feature.
- `description`: What the new feature does and how it was tested.
- `files`: Array containing `internal/tools/<feature>_tool.go`, `internal/tools/<feature>_tool_test.go`, and `internal/tools/custom_tools.go`.

## Step 4: Sandbox Verification & Human Approval
- `propose_self_evolution` automatically runs `go vet ./...`, `go test ./...`, and `go build ./cmd/kawa` inside an isolated Git Worktree + macOS `sandbox-exec` Seatbelt sandbox.
- If any compilation or test error occurs, read the error message, fix your code, and call `propose_self_evolution` again.
- Once tests pass, tell the user the proposal ID (`evolve-...`) so they can reply `/approve <proposal_id>` to automatically merge, recompile, and hot-reload Kawa!
