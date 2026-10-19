package llm

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const grokModelsCacheNote = "讀取自本機 ~/.grok/models_cache.json"

// grokModelsCachePath is the Grok CLI model list. override is for tests.
func grokModelsCachePath(override string) (string, error) {
	if path := strings.TrimSpace(override); path != "" {
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".grok", "models_cache.json"), nil
}

type grokCacheModel struct {
	Info struct {
		ID             string `json:"id"`
		Model          string `json:"model"`
		Name           string `json:"name"`
		Hidden         *bool  `json:"hidden"`
		SupportedInAPI *bool  `json:"supported_in_api"`
	} `json:"info"`
}

func loadGrokModelsCache(override string) ([]ListedModel, error) {
	path, err := grokModelsCachePath(override)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	models, err := parseGrokModelsCache(data)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return models, nil
}

func parseGrokModelsCache(data []byte) ([]ListedModel, error) {
	var file struct {
		Models map[string]grokCacheModel `json:"models"`
	}
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, err
	}
	out := make([]ListedModel, 0, len(file.Models))
	for key, item := range file.Models {
		if item.Info.Hidden != nil && *item.Info.Hidden {
			continue
		}
		if item.Info.SupportedInAPI != nil && !*item.Info.SupportedInAPI {
			continue
		}
		id := strings.TrimSpace(item.Info.ID)
		if id == "" {
			id = strings.TrimSpace(item.Info.Model)
		}
		if id == "" {
			id = strings.TrimSpace(key)
		}
		if id == "" {
			continue
		}
		out = append(out, ListedModel{ID: id, Label: strings.TrimSpace(item.Info.Name)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}
