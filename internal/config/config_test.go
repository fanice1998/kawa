package config

import (
	"path/filepath"
	"testing"
)

func TestLoadExampleConfig(t *testing.T) {
	examplePath := filepath.Join("..", "..", "config.toml.example")
	cfg, err := Load(examplePath)
	if err != nil {
		t.Fatalf("failed to load config.toml.example: %v", err)
	}

	if cfg.Agent.Name != "Kawa" {
		t.Errorf("expected agent.name = Kawa, got %q", cfg.Agent.Name)
	}
	if !cfg.Sandbox.Enabled || !cfg.Sandbox.UseMacOSSeatbelt {
		t.Errorf("expected sandbox enabled and use_macos_seatbelt = true")
	}
	if cfg.LLM.Ollama.BaseURL == "" || cfg.LLM.Claude.Model == "" || cfg.LLM.Gemini.Model == "" {
		t.Errorf("expected LLM providers to be populated from config.toml.example")
	}
	if cfg.LLM.GrokOAuth.TokenURL == "" || cfg.LLM.GeminiOAuth.TokenURL == "" {
		t.Errorf("expected OAuth token URLs to be populated")
	}
}
