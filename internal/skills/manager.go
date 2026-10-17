package skills

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/fanice1998/kawa/internal/sandbox"
)

var validSkillName = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

// Skill represents a reusable agent capability stored in skills/<name>/SKILL.md.
type Skill struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Content     string `json:"content"`
}

// Manager safely manages skills inside a jailed skills directory.
type Manager struct {
	jail *sandbox.FSJail
}

func NewManager(skillsDir string, backupDir string) (*Manager, error) {
	jail, err := sandbox.NewFSJail(skillsDir, backupDir, false)
	if err != nil {
		return nil, fmt.Errorf("create skills jail: %w", err)
	}
	return &Manager{jail: jail}, nil
}

func (m *Manager) ListSkills() ([]Skill, error) {
	entries, err := m.jail.ListDir(".")
	if err != nil {
		return nil, err
	}
	var out []Skill
	for _, entry := range entries {
		if !strings.HasSuffix(entry, "/") {
			continue
		}
		name := strings.TrimSuffix(entry, "/")
		if !validSkillName.MatchString(name) {
			continue
		}
		sk, err := m.ReadSkill(name)
		if err != nil {
			continue
		}
		out = append(out, *sk)
	}
	return out, nil
}

func (m *Manager) ReadSkill(name string) (*Skill, error) {
	if !validSkillName.MatchString(name) {
		return nil, fmt.Errorf("invalid skill name %q (use letters, digits, _, -)", name)
	}
	rel := filepath.Join(name, "SKILL.md")
	data, err := m.jail.ReadFile(rel)
	if err != nil {
		return nil, err
	}
	desc, body := parseFrontmatter(string(data))
	return &Skill{
		Name:        name,
		Description: desc,
		Content:     body,
	}, nil
}

func (m *Manager) SaveSkill(name, description, instructions string) error {
	if !validSkillName.MatchString(name) {
		return fmt.Errorf("invalid skill name %q (use letters, digits, _, -)", name)
	}
	doc := fmt.Sprintf("---\nname: %s\ndescription: %s\n---\n\n%s\n",
		name,
		strings.ReplaceAll(strings.TrimSpace(description), "\n", " "),
		strings.TrimSpace(instructions),
	)
	rel := filepath.Join(name, "SKILL.md")
	return m.jail.WriteFile(rel, []byte(doc))
}

func parseFrontmatter(raw string) (string, string) {
	trimmed := strings.TrimSpace(raw)
	if !strings.HasPrefix(trimmed, "---") {
		return "", trimmed
	}
	rest := strings.TrimPrefix(trimmed, "---")
	idx := strings.Index(rest, "\n---")
	if idx == -1 {
		return "", trimmed
	}
	header := rest[:idx]
	body := strings.TrimSpace(rest[idx+4:])
	var desc string
	for _, line := range strings.Split(header, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "description:") {
			desc = strings.TrimSpace(strings.TrimPrefix(line, "description:"))
			desc = strings.Trim(desc, `"'`)
		}
	}
	return desc, body
}
