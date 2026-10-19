package llm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fanice1998/kawa/internal/config"
)

func disableGrokCache(t *testing.T, p *GrokProvider) {
	t.Helper()
	p.modelsCache = filepath.Join(t.TempDir(), "missing.json")
}

func TestGrokOAuthListModelsFromAPI(t *testing.T) {
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/language-models" || r.Method != http.MethodGet {
			http.NotFound(w, r)
			return
		}
		auth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"models":[{"id":"grok-4","aliases":["grok-latest"]},{"id":"grok-3"}]}`))
	}))
	defer srv.Close()

	p := NewGrokOAuthProvider(config.OAuthModelConfig{
		BaseURL:     srv.URL,
		Model:       "grok-2-latest",
		AccessToken: "tok-1",
		TimeoutSec:  5,
	})
	disableGrokCache(t, p)
	models, err := p.ListModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if auth != "Bearer tok-1" {
		t.Fatalf("auth = %q", auth)
	}
	if len(models) != 2 || models[0].ID != "grok-4" || len(models[0].Aliases) != 1 || models[0].Aliases[0] != "grok-latest" {
		t.Fatalf("models = %#v", models)
	}
	chosen, err := ResolveCatalogModel("grok_oauth", "grok-latest", models)
	if err != nil || chosen != "grok-4" {
		t.Fatalf("alias resolve got %q err=%v", chosen, err)
	}
	if _, err := ResolveCatalogModel("grok_oauth", "not-a-model", models); err == nil {
		t.Fatal("unknown model should be rejected")
	}
	if err := p.SetModel(" "); err == nil {
		t.Fatal("empty model should be rejected")
	}
}

func TestGrokListModelsFallsBackToModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/language-models":
			http.NotFound(w, r)
		case "/models":
			if r.Header.Get("Authorization") != "Bearer key-1" {
				t.Errorf("auth = %q", r.Header.Get("Authorization"))
			}
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"grok-2","aliases":["grok-2-latest"]}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	p := NewGrokAPIKeyProvider(config.APIKeyModelConfig{
		BaseURL:    srv.URL,
		APIKey:     "key-1",
		Model:      "grok-2-latest",
		TimeoutSec: 5,
	})
	disableGrokCache(t, p)
	models, err := p.ListModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 || models[0].ID != "grok-2" {
		t.Fatalf("models = %#v", models)
	}
}

func TestGrokOAuthListModelsRefreshesOn401(t *testing.T) {
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.URL.Path+" "+r.Header.Get("Authorization"))
		switch r.URL.Path {
		case "/oauth/token":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":"new-tok","expires_in":3600}`))
		case "/language-models":
			if r.Header.Get("Authorization") != "Bearer new-tok" {
				http.Error(w, "expired", http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte(`{"models":[{"id":"grok-4"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	p := NewGrokOAuthProvider(config.OAuthModelConfig{
		BaseURL:      srv.URL,
		Model:        "grok-2-latest",
		AccessToken:  "old-tok",
		RefreshToken: "refresh",
		TokenURL:     srv.URL + "/oauth/token",
		TimeoutSec:   5,
	})
	disableGrokCache(t, p)
	models, err := p.ListModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 || models[0].ID != "grok-4" {
		t.Fatalf("models = %#v calls=%v", models, calls)
	}
	if len(calls) < 2 || !strings.Contains(calls[0], "Bearer old-tok") || !strings.Contains(strings.Join(calls, "\n"), "Bearer new-tok") {
		t.Fatalf("calls = %#v", calls)
	}
}

func TestGrokListModelsReadsLocalCache(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		http.Error(w, "should not be called", http.StatusInternalServerError)
	}))
	defer srv.Close()

	dir := t.TempDir()
	path := filepath.Join(dir, "models_cache.json")
	body := []byte(`{
	  "models": {
	    "grok-4.7": {"info": {"id": "grok-4.7", "name": "Grok 4.7", "hidden": false, "supported_in_api": true}},
	    "hidden-one": {"info": {"id": "hidden-one", "name": "Hidden", "hidden": true, "supported_in_api": true}},
	    "embed-only": {"info": {"id": "embed-only", "name": "Embed", "hidden": false, "supported_in_api": false}}
	  }
	}`)
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}

	p := NewGrokOAuthProvider(config.OAuthModelConfig{BaseURL: srv.URL, Model: "grok-2-latest", AccessToken: "tok", TimeoutSec: 5})
	p.modelsCache = path
	models, err := p.ListModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("local cache should be used without calling the API")
	}
	if len(models) != 1 || models[0].ID != "grok-4.7" || models[0].Label != "Grok 4.7" {
		t.Fatalf("models = %#v", models)
	}
	if p.ListNote() != grokModelsCacheNote {
		t.Fatalf("note = %q", p.ListNote())
	}
	text := FormatListedModels(models)
	if !strings.Contains(text, "grok-4.7") || !strings.Contains(text, "Grok 4.7") {
		t.Fatalf("format = %q", text)
	}
}

func TestClaudeAndGeminiListModelsFromAPI(t *testing.T) {
	claudeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("x-api-key") != "sk-test" {
			t.Errorf("claude key = %q", r.Header.Get("x-api-key"))
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"claude-sonnet-4-5","type":"model"}],"has_more":false}`))
	}))
	defer claudeSrv.Close()

	geminiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("x-goog-api-key") != "gem-key" {
			t.Errorf("gemini key = %q", r.Header.Get("x-goog-api-key"))
		}
		_, _ = w.Write([]byte(`{"models":[{"name":"models/gemini-2.5-pro","supportedGenerationMethods":["generateContent"]},{"name":"models/text-embed","supportedGenerationMethods":["embedContent"]}]}`))
	}))
	defer geminiSrv.Close()

	claude := NewClaudeProvider(config.ClaudeConfig{BaseURL: claudeSrv.URL, APIKey: "sk-test", Model: "old", TimeoutSec: 5})
	got, err := claude.ListModels(context.Background())
	if err != nil || len(got) != 1 || got[0].ID != "claude-sonnet-4-5" {
		t.Fatalf("claude models = %#v err=%v", got, err)
	}

	gemini := NewGeminiAPIKeyProvider(config.APIKeyModelConfig{BaseURL: geminiSrv.URL, APIKey: "gem-key", Model: "old", TimeoutSec: 5})
	got, err = gemini.ListModels(context.Background())
	if err != nil || len(got) != 1 || got[0].ID != "gemini-2.5-pro" {
		t.Fatalf("gemini models = %#v err=%v", got, err)
	}
}

func TestApplyModelStateRestoresProviderAndModel(t *testing.T) {
	reg := NewRegistry(config.LLMConfig{
		Ollama:    config.OllamaConfig{Model: "config-ollama"},
		GrokOAuth: config.OAuthModelConfig{Model: "config-grok"},
	}, "ollama")
	st := ModelState{ActiveProvider: "grok_oauth", OllamaModel: "qwen3.6:27b"}
	st.Remember("grok_oauth", "grok-4")

	active, restored := ApplyModelState(reg, nil, st)
	if active != "grok_oauth" || reg.ActiveName() != "grok_oauth" {
		t.Fatalf("active = %q registry=%q", active, reg.ActiveName())
	}
	grok, ok := reg.Get("grok_oauth")
	if !ok || grok.Model() != "grok-4" {
		t.Fatalf("grok model = %q ok=%v", grok.Model(), ok)
	}
	ollama, ok := reg.Ollama()
	if !ok || ollama.Model() != "qwen3.6:27b" {
		t.Fatalf("ollama model ok=%v", ok)
	}
	if len(restored) != 2 {
		t.Fatalf("restored = %#v", restored)
	}
}
