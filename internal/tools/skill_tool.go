package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/fanice1998/kawa/internal/skills"
)

// ListSkillsTool lists all installed skills in `skills/<name>/SKILL.md`.
type ListSkillsTool struct {
	mgr *skills.Manager
}

func NewListSkillsTool(mgr *skills.Manager) *ListSkillsTool {
	return &ListSkillsTool{mgr: mgr}
}

func (t *ListSkillsTool) Name() string { return "list_skills" }
func (t *ListSkillsTool) Description() string {
	return "List all reusable agent skills stored in the skills directory."
}
func (t *ListSkillsTool) Parameters() json.RawMessage {
	return json.RawMessage(`{"type": "object", "properties": {}}`)
}

func (t *ListSkillsTool) Execute(_ context.Context, _ string) (string, error) {
	list, err := t.mgr.ListSkills()
	if err != nil {
		return "", err
	}
	if len(list) == 0 {
		return "No skills installed yet.", nil
	}
	var sb strings.Builder
	for _, s := range list {
		sb.WriteString(fmt.Sprintf("- %s: %s\n", s.Name, s.Description))
	}
	return strings.TrimSpace(sb.String()), nil
}

// ReadSkillTool reads the full markdown instructions of a specific skill.
type ReadSkillTool struct {
	mgr *skills.Manager
}

func NewReadSkillTool(mgr *skills.Manager) *ReadSkillTool {
	return &ReadSkillTool{mgr: mgr}
}

func (t *ReadSkillTool) Name() string { return "read_skill" }
func (t *ReadSkillTool) Description() string {
	return "Read the detailed instructions (SKILL.md) of a specific skill by name."
}
func (t *ReadSkillTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"name": {
				"type": "string",
				"description": "Skill folder name (e.g. 'self_improvement')."
			}
		},
		"required": ["name"]
	}`)
}

func (t *ReadSkillTool) Execute(_ context.Context, rawArgs string) (string, error) {
	var args struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal([]byte(rawArgs), &args); err != nil {
		return "", err
	}
	sk, err := t.mgr.ReadSkill(args.Name)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("# Skill: %s\nDescription: %s\n\n%s", sk.Name, sk.Description, sk.Content), nil
}

// SaveSkillTool creates or updates a reusable skill in `skills/<name>/SKILL.md`.
type SaveSkillTool struct {
	mgr *skills.Manager
}

func NewSaveSkillTool(mgr *skills.Manager) *SaveSkillTool {
	return &SaveSkillTool{mgr: mgr}
}

func (t *SaveSkillTool) Name() string { return "save_skill" }
func (t *SaveSkillTool) Description() string {
	return "Create or update a reusable skill (skills/<name>/SKILL.md) with a description and step-by-step instructions."
}
func (t *SaveSkillTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"name": {
				"type": "string",
				"description": "Skill identifier using letters, numbers, underscores, or hyphens."
			},
			"description": {
				"type": "string",
				"description": "One-line summary of when and how to use this skill."
			},
			"instructions": {
				"type": "string",
				"description": "Detailed Markdown instructions, workflows, or examples for this skill."
			}
		},
		"required": ["name", "description", "instructions"]
	}`)
}

func (t *SaveSkillTool) Execute(_ context.Context, rawArgs string) (string, error) {
	var args struct {
		Name         string `json:"name"`
		Description  string `json:"description"`
		Instructions string `json:"instructions"`
	}
	if err := json.Unmarshal([]byte(rawArgs), &args); err != nil {
		return "", err
	}
	if err := t.mgr.SaveSkill(args.Name, args.Description, args.Instructions); err != nil {
		return "", err
	}
	return fmt.Sprintf("Skill %q saved successfully to skills/%s/SKILL.md", args.Name, args.Name), nil
}
