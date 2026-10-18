package llm

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

// ModelState is the last Ollama model chosen with /model.
// It overrides config.toml after a restart.
type ModelState struct {
	OllamaModel string `json:"ollama_model"`
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
