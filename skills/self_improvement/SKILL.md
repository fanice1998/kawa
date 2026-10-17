---
name: self_improvement
description: Step-by-step guide for Kawa to safely inspect its own Go source code, write new features/tools, verify in the Seatbelt sandbox, and propose self-evolution.
---

# Kawa Self-Improvement & Code Evolution Skill

When the user asks you to add a new feature, create a new tool, optimize existing code, or fix a bug in yourself, follow this strict safety workflow:

## Step 1: Inspect Current Source Code
1. Use `list_dir` with `{"scope": "source", "path": "."}` or `{"scope": "source", "path": "internal/tools"}` to explore the codebase structure.
2. Use `read_file` with `{"scope": "source", "path": "internal/tools/tool.go"}` (and related files) to understand interfaces and conventions.

## Step 2: Design & Write Unit Tests
- Every self-modification MUST compile cleanly (`go vet ./...`) and pass all unit tests (`go test ./...`).
- Whenever you add a new function or tool, include a corresponding `*_test.go` file in your proposal.
- Never attempt to modify or delete files outside the repository; the macOS `sandbox-exec` Seatbelt kernel profile enforces `(deny file-write*)` outside the isolated worktree.

## Step 3: Submit via `propose_self_evolution`
Call `propose_self_evolution` with:
- `title`: A short summary of the improvement.
- `description`: Explanation of the changes and tests added.
- `files`: Array of `{ "path": "relative/path.go", "content": "full Go file content" }`.

## Step 4: Handle Sandbox Results & Request Human Approval
- If `propose_self_evolution` returns a compiler or test error, carefully read the error output, fix the code, and call `propose_self_evolution` again.
- Once `propose_self_evolution` succeeds, present the proposal ID and diff summary to the user so they can reply `/approve <proposal_id>` to merge and activate the update.
