package evolution

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// ProposeEvolutionTool exposes the safe self-modification pipeline as an LLM Tool.
type ProposeEvolutionTool struct {
	engine   *Engine
	notifyFn func(p *Proposal)
}

func NewProposeEvolutionTool(engine *Engine, notifyFn func(p *Proposal)) *ProposeEvolutionTool {
	return &ProposeEvolutionTool{
		engine:   engine,
		notifyFn: notifyFn,
	}
}

func (t *ProposeEvolutionTool) Name() string { return "propose_self_evolution" }
func (t *ProposeEvolutionTool) Description() string {
	return "Safely propose a modification, bugfix, optimization, or new tool/feature to Kawa's own Go source code. Runs `go vet`, `go test`, and `go build` inside an isolated Git Worktree + macOS Seatbelt sandbox, and sends the diff to the human user for `/approve <id>` confirmation."
}
func (t *ProposeEvolutionTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"title": {
				"type": "string",
				"description": "Concise title of the self-evolution change."
			},
			"description": {
				"type": "string",
				"description": "Detailed explanation of what was changed/added and how it is tested."
			},
			"files": {
				"type": "array",
				"description": "List of complete source files to create or update inside the repository.",
				"items": {
					"type": "object",
					"properties": {
						"path": {
							"type": "string",
							"description": "Relative file path in the repo (e.g. 'internal/tools/math_tool.go')."
						},
						"content": {
							"type": "string",
							"description": "Complete Go source code for this file."
						}
					},
					"required": ["path", "content"]
				}
			}
		},
		"required": ["title", "description", "files"]
	}`)
}

func (t *ProposeEvolutionTool) Execute(ctx context.Context, rawArgs string) (string, error) {
	var args struct {
		Title       string      `json:"title"`
		Description string      `json:"description"`
		Files       []FilePatch `json:"files"`
	}
	if err := json.Unmarshal([]byte(rawArgs), &args); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	p, err := t.engine.Propose(ctx, args.Title, args.Description, args.Files)
	if err != nil {
		return "", err
	}

	if t.notifyFn != nil {
		t.notifyFn(p)
	}

	diffPreview := p.Diff
	if len(diffPreview) > 3000 {
		diffPreview = diffPreview[:3000] + "\n...[diff truncated]"
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Self-evolution proposal %q (%s) passed all sandbox tests (`go vet`, `go test`, `go build`)!\n", p.ID, p.Title))
	sb.WriteString(fmt.Sprintf("Status: %s\n", p.Status))
	sb.WriteString(fmt.Sprintf("Ask the user to review and reply with `/approve %s` to apply, or `/reject %s` to discard.\n\n", p.ID, p.ID))
	sb.WriteString("--- Git Diff Summary ---\n")
	sb.WriteString(diffPreview)
	return sb.String(), nil
}
