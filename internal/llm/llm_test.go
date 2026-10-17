package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/fanice1998/kawa/internal/config"
)

func TestOAuthAutoRefreshAndProviders(t *testing.T) {
	var refreshCount int32

	mux := http.NewServeMux()
	mux.HandleFunc("/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&refreshCount, 1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "fresh-oauth-token-123",
			"refresh_token": "refresh-xyz",
			"expires_in":    3600,
		})
	})

	// Mock Grok OAuth endpoint that rejects stale token with 401 and accepts fresh-oauth-token-123
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fresh-oauth-token-123" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"expired token"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"choices": [{
				"message": {
					"role": "assistant",
					"content": "Hello from Grok OAuth!",
					"tool_calls": [{
						"id": "call_1",
						"type": "function",
						"function": {"name": "read_file", "arguments": "{\"path\":\"README.md\"}"}
					}]
				}
			}]
		}`))
	})

	// Mock Gemini OAuth endpoint
	mux.HandleFunc("/v1beta/models/gemini-2.5-pro:generateContent", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fresh-oauth-token-123" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"candidates": [{
				"content": {
					"role": "model",
					"parts": [
						{"text": "Hello from Gemini OAuth!"},
						{"functionCall": {"name": "web_search", "args": {"query": "golang os.Root"}}}
					]
				}
			}]
		}`))
	})

	// Mock Ollama endpoint returning inline JSON tool call in message.content (like qwen2.5-coder)
	mux.HandleFunc("/api/chat", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"message": {
				"role": "assistant",
				"content": "{\"name\": \"run_self_test\", \"arguments\": {\"package\": \"./...\"}}"
			}
		}`))
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	grokOAuth := NewGrokOAuthProvider(config.OAuthModelConfig{
		BaseURL:      server.URL + "/v1",
		Model:        "grok-2-latest",
		AccessToken:  "stale-token",
		RefreshToken: "refresh-xyz",
		TokenURL:     server.URL + "/oauth/token",
		ExpiresAt:    "2099-01-01T00:00:00Z", // Not expired locally, so it tests 401 auto-refresh retry!
	})

	resp, err := grokOAuth.Chat(context.Background(), ChatRequest{
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("GrokOAuth Chat failed: %v", err)
	}
	if resp.Content != "Hello from Grok OAuth!" || len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Name != "read_file" {
		t.Fatalf("unexpected GrokOAuth response: %+v", resp)
	}
	if atomic.LoadInt32(&refreshCount) != 1 {
		t.Fatalf("expected 1 OAuth token refresh on 401, got %d", refreshCount)
	}

	geminiOAuth := NewGeminiOAuthProvider(config.OAuthModelConfig{
		BaseURL:      server.URL + "/v1beta",
		Model:        "gemini-2.5-pro",
		AccessToken:  "expired-token",
		RefreshToken: "refresh-xyz",
		TokenURL:     server.URL + "/oauth/token",
		ExpiresAt:    "2020-01-01T00:00:00Z", // Expired timestamp -> proactive refresh before call
	})

	gResp, err := geminiOAuth.Chat(context.Background(), ChatRequest{
		Messages: []Message{{Role: RoleUser, Content: "search"}},
	})
	if err != nil {
		t.Fatalf("GeminiOAuth Chat failed: %v", err)
	}
	if gResp.Content != "Hello from Gemini OAuth!" || len(gResp.ToolCalls) != 1 || gResp.ToolCalls[0].Name != "web_search" {
		t.Fatalf("unexpected GeminiOAuth response: %+v", gResp)
	}

	ollamaProv := NewOllamaProvider(config.OllamaConfig{
		BaseURL: server.URL,
		Model:   "qwen2.5-coder:14b",
	})
	oResp, err := ollamaProv.Chat(context.Background(), ChatRequest{
		Messages: []Message{{Role: RoleUser, Content: "test yourself"}},
		Tools: []ToolDefinition{
			{Name: "run_self_test", Description: "run tests", Parameters: json.RawMessage(`{"type":"object"}`)},
		},
	})
	if err != nil {
		t.Fatalf("Ollama Chat failed: %v", err)
	}
	if oResp.Content != "" || len(oResp.ToolCalls) != 1 || oResp.ToolCalls[0].Name != "run_self_test" {
		t.Fatalf("expected inline JSON tool call to be extracted into ToolCalls, got: %+v", oResp)
	}
}
