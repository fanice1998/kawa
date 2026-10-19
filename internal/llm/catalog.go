package llm

import (
	"context"
	"strings"

	"github.com/fanice1998/kawa/internal/config"
)

// ModelCatalog is a provider that can list and switch models through its own API.
type ModelCatalog interface {
	Name() string
	Model() string
	SetModel(name string) error
	ListModels(ctx context.Context) ([]ListedModel, error)
}

type ollamaCatalog struct {
	p *OllamaProvider
}

func (o ollamaCatalog) Name() string               { return o.p.Name() }
func (o ollamaCatalog) Model() string              { return o.p.Model() }
func (o ollamaCatalog) SetModel(name string) error { return o.p.SetModel(name) }
func (o ollamaCatalog) ListModels(ctx context.Context) ([]ListedModel, error) {
	names, err := o.p.ListModels(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]ListedModel, 0, len(names))
	for _, name := range names {
		out = append(out, ListedModel{ID: name})
	}
	return out, nil
}

// CatalogNote is a short line about where the last ListModels result came from.
func CatalogNote(cat ModelCatalog) string {
	type noter interface {
		ListNote() string
	}
	if n, ok := cat.(noter); ok {
		return n.ListNote()
	}
	return ""
}

// Catalog returns the model listing for this provider.
func Catalog(p Provider) (ModelCatalog, bool) {
	if p == nil {
		return nil, false
	}
	switch v := p.(type) {
	case *OllamaProvider:
		return ollamaCatalog{p: v}, true
	case ModelCatalog:
		return v, true
	default:
		return nil, false
	}
}

// ResolveCatalogModel checks want against the models just returned by that provider.
func ResolveCatalogModel(providerName, want string, available []ListedModel) (string, error) {
	if providerName == "ollama" {
		names := make([]string, 0, len(available))
		for _, m := range available {
			names = append(names, m.ID)
		}
		return ResolveOllamaModel(want, names)
	}
	return ResolveListedModel(providerName, want, available)
}

// AssignConfigModel mirrors a switched model into the in-memory config.
func AssignConfigModel(cfg *config.Config, provider, model string) {
	if cfg == nil {
		return
	}
	switch provider {
	case "ollama":
		cfg.LLM.Ollama.Model = model
	case "xai_grok":
		cfg.LLM.XAIGrok.Model = model
	case "grok_oauth":
		cfg.LLM.GrokOAuth.Model = model
	case "claude":
		cfg.LLM.Claude.Model = model
	case "gemini":
		cfg.LLM.Gemini.Model = model
	case "gemini_oauth":
		cfg.LLM.GeminiOAuth.Model = model
	}
}

// ApplyModelState restores the last provider and per-provider models after restart.
// restored entries look like "grok_oauth=grok-4".
func ApplyModelState(reg *Registry, cfg *config.Config, st ModelState) (active string, restored []string) {
	if reg == nil {
		return "", nil
	}
	if name := strings.TrimSpace(st.ActiveProvider); name != "" {
		if err := reg.SetActive(name); err == nil {
			active = name
			if cfg != nil {
				cfg.Agent.DefaultProvider = name
			}
		}
	}
	for _, name := range []string{"ollama", "xai_grok", "grok_oauth", "claude", "gemini", "gemini_oauth"} {
		model := st.ModelOf(name)
		if model == "" {
			continue
		}
		p, ok := reg.Get(name)
		if !ok {
			continue
		}
		setter, ok := p.(interface{ SetModel(string) error })
		if !ok {
			continue
		}
		if err := setter.SetModel(model); err != nil {
			continue
		}
		AssignConfigModel(cfg, name, model)
		restored = append(restored, name+"="+model)
	}
	return active, restored
}
