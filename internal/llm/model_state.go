package llm

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// ModelState is the last /provider and /model choice.
// It overrides config.toml after a restart. ollama_model stays so older files still load.
type ModelState struct {
	ActiveProvider string            `json:"active_provider,omitempty"`
	OllamaModel    string            `json:"ollama_model,omitempty"`
	Models         map[string]string `json:"models,omitempty"`
}

// ModelOf returns the saved model for a provider.
func (st ModelState) ModelOf(provider string) string {
	if st.Models != nil {
		if model := strings.TrimSpace(st.Models[provider]); model != "" {
			return model
		}
	}
	if provider == "ollama" {
		return strings.TrimSpace(st.OllamaModel)
	}
	return ""
}

// Remember stores one provider's model without dropping the others.
func (st *ModelState) Remember(provider, model string) {
	provider = strings.TrimSpace(provider)
	model = strings.TrimSpace(model)
	if st.Models == nil {
		st.Models = map[string]string{}
	}
	st.Models[provider] = model
	if provider == "ollama" {
		st.OllamaModel = model
	}
}

func ModelStatePath(stateDir string) string {
	return filepath.Join(stateDir, "llm_state.json")
}

func LoadModelState(stateDir string) (ModelState, error) {
	var st ModelState
	if stateDir == "" {
		return st, nil
	}
	data, err := os.ReadFile(ModelStatePath(stateDir))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return st, nil
		}
		return st, err
	}
	if err := json.Unmarshal(data, &st); err != nil {
		return ModelState{}, err
	}
	return st, nil
}

func SaveModelState(stateDir string, st ModelState) error {
	if stateDir == "" {
		return errors.New("state dir is empty")
	}
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	return os.WriteFile(ModelStatePath(stateDir), raw, 0o644)
}

// PersistActiveProvider records the provider that should be active after restart.
func PersistActiveProvider(stateDir, provider string) error {
	st, err := LoadModelState(stateDir)
	if err != nil {
		return err
	}
	st.ActiveProvider = strings.TrimSpace(provider)
	return SaveModelState(stateDir, st)
}

// PersistModelChoice records one provider's model and keeps every other saved choice.
func PersistModelChoice(stateDir, provider, model string) error {
	st, err := LoadModelState(stateDir)
	if err != nil {
		return err
	}
	st.Remember(provider, model)
	return SaveModelState(stateDir, st)
}
