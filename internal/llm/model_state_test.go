package llm

import (
	"testing"

	"github.com/fanice1998/kawa/internal/config"
)

func TestOllamaModelSwitchPersists(t *testing.T) {
	p := NewOllamaProvider(config.OllamaConfig{BaseURL: "http://127.0.0.1:11434", Model: "qwen3.6:27b"})
	if err := p.SetModel("qwen2.5-coder:14b"); err != nil {
		t.Fatal(err)
	}
	if p.Model() != "qwen2.5-coder:14b" {
		t.Fatalf("model = %s", p.Model())
	}
	if err := p.SetModel("  "); err == nil {
		t.Fatal("empty model should be rejected")
	}

	dir := t.TempDir()
	if err := SaveModelState(dir, ModelState{OllamaModel: p.Model()}); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadModelState(dir)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.OllamaModel != "qwen2.5-coder:14b" {
		t.Fatalf("loaded %#v", loaded)
	}
	missing, err := LoadModelState(t.TempDir())
	if err != nil || missing.OllamaModel != "" {
		t.Fatalf("missing state = %#v err=%v", missing, err)
	}
}

func TestResolveOllamaModel(t *testing.T) {
	available := []string{"qwen3.6:27b", "qwen2.5-coder:14b"}
	got, err := ResolveOllamaModel("qwen2.5-coder", available)
	if err != nil || got != "qwen2.5-coder:14b" {
		t.Fatalf("got %q err=%v", got, err)
	}
	if _, err := ResolveOllamaModel("missing", available); err == nil {
		t.Fatal("expected unknown model to be rejected")
	}
	got, err = ResolveOllamaModel("custom:latest", nil)
	if err != nil || got != "custom:latest" {
		t.Fatalf("unverified name got %q err=%v", got, err)
	}
}
