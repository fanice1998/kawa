package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/fanice1998/kawa/internal/memory"
)

// MemorySaveTool allows Kawa to actively save facts, user preferences, or project notes into Core Memory & RAG.
type MemorySaveTool struct {
	mem *memory.Engine
}

func NewMemorySaveTool(mem *memory.Engine) *MemorySaveTool {
	return &MemorySaveTool{mem: mem}
}

func (t *MemorySaveTool) Name() string { return "memory_save" }
func (t *MemorySaveTool) Description() string {
	return "Save an important fact, user preference, ongoing project state, or technical note into long-term RAG memory (and MEMORY.md if category is 'core' or 'preference')."
}
func (t *MemorySaveTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"category": {
				"type": "string",
				"enum": ["core", "preference", "fact", "task"],
				"description": "Memory category: 'core' or 'preference' for permanent user/system facts, 'fact' or 'task' for searchable RAG knowledge."
			},
			"content": {
				"type": "string",
				"description": "The clear, self-contained fact or information to remember."
			}
		},
		"required": ["content"]
	}`)
}

func (t *MemorySaveTool) Execute(_ context.Context, rawArgs string) (string, error) {
	var args struct {
		Category string `json:"category"`
		Content  string `json:"content"`
	}
	if err := json.Unmarshal([]byte(rawArgs), &args); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	if args.Category == "" {
		args.Category = "core"
	}
	entry, err := t.mem.Add(args.Category, args.Content, "agent_tool")
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("Saved memory [%s] (%s): %s", entry.ID, entry.Category, entry.Content), nil
}

// MemorySearchTool allows Kawa to actively query the RAG memory store.
type MemorySearchTool struct {
	mem *memory.Engine
}

func NewMemorySearchTool(mem *memory.Engine) *MemorySearchTool {
	return &MemorySearchTool{mem: mem}
}

func (t *MemorySearchTool) Name() string { return "memory_search" }
func (t *MemorySearchTool) Description() string {
	return "Search long-term RAG memory and past conversation history for relevant context, user preferences, or prior tasks."
}
func (t *MemorySearchTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"query": {
				"type": "string",
				"description": "Keywords or natural language query to search in RAG memory."
			},
			"top_k": {
				"type": "integer",
				"description": "Maximum number of results to return (default 5)."
			}
		},
		"required": ["query"]
	}`)
}

func (t *MemorySearchTool) Execute(_ context.Context, rawArgs string) (string, error) {
	var args struct {
		Query string `json:"query"`
		TopK  int    `json:"top_k"`
	}
	if err := json.Unmarshal([]byte(rawArgs), &args); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	results := t.mem.Search(args.Query, args.TopK)
	if len(results) == 0 {
		return fmt.Sprintf("No matching memories found for query %q.", args.Query), nil
	}
	var sb strings.Builder
	for i, r := range results {
		sb.WriteString(fmt.Sprintf("%d. [%s | %s | score=%.2f | %s]\n%s\n\n",
			i+1, r.Entry.ID, r.Entry.Category, r.Score, r.Entry.CreatedAt.Format("2006-01-02 15:04"), r.Entry.Content))
	}
	return strings.TrimSpace(sb.String()), nil
}

// MemoryDeleteTool removes a memory entry by ID.
type MemoryDeleteTool struct {
	mem *memory.Engine
}

func NewMemoryDeleteTool(mem *memory.Engine) *MemoryDeleteTool {
	return &MemoryDeleteTool{mem: mem}
}

func (t *MemoryDeleteTool) Name() string { return "memory_delete" }
func (t *MemoryDeleteTool) Description() string {
	return "Delete an outdated or incorrect memory entry from the RAG store by its ID."
}
func (t *MemoryDeleteTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"id": {
				"type": "string",
				"description": "Memory ID (e.g. 'mem-1729200000')."
			}
		},
		"required": ["id"]
	}`)
}

func (t *MemoryDeleteTool) Execute(_ context.Context, rawArgs string) (string, error) {
	var args struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(rawArgs), &args); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	if t.mem.Delete(strings.TrimSpace(args.ID)) {
		return fmt.Sprintf("Deleted memory %s.", args.ID), nil
	}
	return fmt.Sprintf("Memory ID %s not found.", args.ID), nil
}
