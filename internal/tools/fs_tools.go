package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/fanice1998/kawa/internal/sandbox"
)

// ReadFileTool reads a file from either the `workspace` jail or the `source` jail (read-only).
type ReadFileTool struct {
	workspaceJail *sandbox.FSJail
	sourceJail    *sandbox.FSJail
}

func NewReadFileTool(workspaceJail, sourceJail *sandbox.FSJail) *ReadFileTool {
	return &ReadFileTool{
		workspaceJail: workspaceJail,
		sourceJail:    sourceJail,
	}
}

func (t *ReadFileTool) Name() string { return "read_file" }
func (t *ReadFileTool) Description() string {
	return "Safely read a file inside 'workspace' (user files) or 'source' (Kawa's own Go source code for inspection)."
}
func (t *ReadFileTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"scope": {
				"type": "string",
				"enum": ["workspace", "source"],
				"description": "Target scope: 'workspace' for general working files, or 'source' to inspect Kawa's own source code."
			},
			"path": {
				"type": "string",
				"description": "Relative file path inside the chosen scope (e.g. 'notes.txt' or 'internal/agent/agent.go')."
			}
		},
		"required": ["path"]
	}`)
}

func (t *ReadFileTool) Execute(_ context.Context, rawArgs string) (string, error) {
	var args struct {
		Scope string `json:"scope"`
		Path  string `json:"path"`
	}
	if err := json.Unmarshal([]byte(rawArgs), &args); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	jail := t.workspaceJail
	if strings.EqualFold(args.Scope, "source") {
		jail = t.sourceJail
	}
	data, err := jail.ReadFile(args.Path)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// WriteFileTool writes a file strictly inside the `workspace` jail.
// Note: To modify Kawa's own source code, the agent must use `propose_self_evolution`.
type WriteFileTool struct {
	workspaceJail *sandbox.FSJail
}

func NewWriteFileTool(workspaceJail *sandbox.FSJail) *WriteFileTool {
	return &WriteFileTool{workspaceJail: workspaceJail}
}

func (t *WriteFileTool) Name() string { return "write_file" }
func (t *WriteFileTool) Description() string {
	return "Safely write or create a file inside the jailed workspace directory. Existing files are backed up automatically. To modify Kawa's own Go source code, use 'propose_self_evolution' instead."
}
func (t *WriteFileTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"path": {
				"type": "string",
				"description": "Relative file path inside workspace (e.g. 'report.md' or 'data/output.json')."
			},
			"content": {
				"type": "string",
				"description": "Full text content to write to the file."
			}
		},
		"required": ["path", "content"]
	}`)
}

func (t *WriteFileTool) Execute(_ context.Context, rawArgs string) (string, error) {
	var args struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal([]byte(rawArgs), &args); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	if err := t.workspaceJail.WriteFile(args.Path, []byte(args.Content)); err != nil {
		return "", err
	}
	return fmt.Sprintf("Successfully wrote %d bytes to workspace/%s", len(args.Content), args.Path), nil
}

// ListDirTool lists directory entries inside either `workspace` or `source` jail.
type ListDirTool struct {
	workspaceJail *sandbox.FSJail
	sourceJail    *sandbox.FSJail
}

func NewListDirTool(workspaceJail, sourceJail *sandbox.FSJail) *ListDirTool {
	return &ListDirTool{
		workspaceJail: workspaceJail,
		sourceJail:    sourceJail,
	}
}

func (t *ListDirTool) Name() string { return "list_dir" }
func (t *ListDirTool) Description() string {
	return "List files and subdirectories inside 'workspace' or 'source' (Kawa's own repository)."
}
func (t *ListDirTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"scope": {
				"type": "string",
				"enum": ["workspace", "source"],
				"description": "Target scope: 'workspace' or 'source'."
			},
			"path": {
				"type": "string",
				"description": "Relative directory path (use '.' for root)."
			}
		},
		"required": ["path"]
	}`)
}

func (t *ListDirTool) Execute(_ context.Context, rawArgs string) (string, error) {
	var args struct {
		Scope string `json:"scope"`
		Path  string `json:"path"`
	}
	if err := json.Unmarshal([]byte(rawArgs), &args); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	if strings.TrimSpace(args.Path) == "" {
		args.Path = "."
	}
	jail := t.workspaceJail
	if strings.EqualFold(args.Scope, "source") {
		jail = t.sourceJail
	}
	entries, err := jail.ListDir(args.Path)
	if err != nil {
		return "", err
	}
	if len(entries) == 0 {
		return "(empty directory)", nil
	}
	return strings.Join(entries, "\n"), nil
}
