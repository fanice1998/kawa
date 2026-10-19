package llm

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ListedModel is one model returned by a provider API or a local cache.
type ListedModel struct {
	ID      string
	Aliases []string
	Label   string
}

type modelWire struct {
	ID                         string   `json:"id"`
	Name                       string   `json:"name"`
	Aliases                    []string `json:"aliases"`
	SupportedGenerationMethods []string `json:"supportedGenerationMethods"`
}

type modelListEnvelope struct {
	Models        []modelWire `json:"models"`
	Data          []modelWire `json:"data"`
	HasMore       bool        `json:"has_more"`
	LastID        string      `json:"last_id"`
	NextPageToken string      `json:"nextPageToken"`
}

func (w modelWire) listedID() string {
	id := strings.TrimSpace(w.ID)
	if id == "" {
		id = strings.TrimSpace(w.Name)
	}
	return strings.TrimPrefix(id, "models/")
}

func decodeModelPage(body []byte, chatOnly bool) (modelListEnvelope, []ListedModel, error) {
	var env modelListEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return env, nil, fmt.Errorf("decode model list: %w", err)
	}
	raw := env.Models
	if len(raw) == 0 {
		raw = env.Data
	}
	var out []ListedModel
	for _, w := range raw {
		if chatOnly && !supportsChat(w.SupportedGenerationMethods) {
			continue
		}
		id := w.listedID()
		if id == "" {
			continue
		}
		out = append(out, ListedModel{ID: id, Aliases: cleanAliases(id, w.Aliases)})
	}
	return env, out, nil
}

func supportsChat(methods []string) bool {
	if len(methods) == 0 {
		return true
	}
	for _, m := range methods {
		if m == "generateContent" {
			return true
		}
	}
	return false
}

func cleanAliases(id string, aliases []string) []string {
	var out []string
	seen := map[string]bool{id: true}
	for _, a := range aliases {
		a = strings.TrimSpace(a)
		if a == "" || seen[a] {
			continue
		}
		seen[a] = true
		out = append(out, a)
	}
	return out
}

func dedupeListed(in []ListedModel) []ListedModel {
	index := map[string]int{}
	var out []ListedModel
	for _, m := range in {
		id := strings.TrimSpace(m.ID)
		if id == "" {
			continue
		}
		aliases := cleanAliases(id, m.Aliases)
		if i, ok := index[id]; ok {
			out[i].Aliases = cleanAliases(id, append(out[i].Aliases, aliases...))
			if out[i].Label == "" {
				out[i].Label = strings.TrimSpace(m.Label)
			}
			continue
		}
		index[id] = len(out)
		out = append(out, ListedModel{ID: id, Aliases: aliases, Label: strings.TrimSpace(m.Label)})
	}
	return out
}

// ResolveListedModel matches want against ids and aliases from a live list.
// An alias resolves to the canonical id returned by the API.
func ResolveListedModel(providerName, want string, available []ListedModel) (string, error) {
	want = strings.TrimSpace(want)
	if want == "" {
		return "", fmt.Errorf("模型名稱是空的")
	}
	if len(available) == 0 {
		return "", fmt.Errorf("%s 的模型清單是空的", providerName)
	}
	for _, m := range available {
		if m.ID == want {
			return m.ID, nil
		}
		for _, alias := range m.Aliases {
			if alias == want {
				return m.ID, nil
			}
		}
	}
	return "", fmt.Errorf("%s 沒有模型 %q。可用：%s", providerName, want, modelNameSummary(available))
}

func modelNameSummary(models []ListedModel) string {
	var names []string
	for _, m := range models {
		if m.ID != "" {
			names = append(names, m.ID)
		}
		names = append(names, m.Aliases...)
	}
	if len(names) > 30 {
		return strings.Join(names[:30], ", ") + fmt.Sprintf(" …共 %d 個", len(names))
	}
	return strings.Join(names, ", ")
}

// FormatListedModels renders API models for the /model reply.
func FormatListedModels(models []ListedModel) string {
	var b strings.Builder
	for _, m := range models {
		if m.ID == "" {
			continue
		}
		line := "• `" + m.ID + "`"
		if label := strings.TrimSpace(m.Label); label != "" && label != m.ID {
			line += " — " + label
		}
		if len(m.Aliases) > 0 {
			quoted := make([]string, len(m.Aliases))
			for i, a := range m.Aliases {
				quoted[i] = "`" + a + "`"
			}
			line += "（別名 " + strings.Join(quoted, "、") + "）"
		}
		b.WriteString(line + "\n")
	}
	return strings.TrimSpace(b.String())
}

func clipBody(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 300 {
		return s[:300]
	}
	return s
}
