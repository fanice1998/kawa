package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/fanice1998/kawa/internal/config"
)

type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// ToolDefinition describes a tool exposed to the LLM via JSON Schema.
type ToolDefinition struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// ToolCall represents a function/tool invocation requested by the LLM.
type ToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// Message represents a single turn in the conversation history.
type Message struct {
	Role       Role       `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	ToolName   string     `json:"tool_name,omitempty"`
}

// ChatRequest contains messages and available tools for an LLM turn.
type ChatRequest struct {
	Messages []Message
	Tools    []ToolDefinition
}

// ChatResponse is the normalized response from any LLM provider.
type ChatResponse struct {
	Content   string
	ToolCalls []ToolCall
	Provider  string
	Model     string
}

// Provider is the unified interface implemented by all AI backends.
type Provider interface {
	Name() string
	Model() string
	Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error)
}

// Registry manages all configured LLM providers and tracks the active one.
type Registry struct {
	mu        sync.RWMutex
	providers map[string]Provider
	active    string
}

// NewRegistry initializes all 6 required providers from config.
func NewRegistry(cfg config.LLMConfig, defaultProvider string) *Registry {
	r := &Registry{
		providers: make(map[string]Provider),
	}

	r.Register(NewOllamaProvider(cfg.Ollama))
	r.Register(NewGrokAPIKeyProvider(cfg.XAIGrok))
	r.Register(NewGrokOAuthProvider(cfg.GrokOAuth))
	r.Register(NewClaudeProvider(cfg.Claude))
	r.Register(NewGeminiAPIKeyProvider(cfg.Gemini))
	r.Register(NewGeminiOAuthProvider(cfg.GeminiOAuth))

	if _, ok := r.providers[defaultProvider]; ok {
		r.active = defaultProvider
	} else {
		r.active = "ollama"
	}
	return r
}

func (r *Registry) Register(p Provider) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.providers[p.Name()] = p
}

func (r *Registry) Active() (Provider, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.providers[r.active]
	if !ok {
		return nil, fmt.Errorf("active provider %q not found", r.active)
	}
	return p, nil
}

func (r *Registry) SetActive(name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.providers[name]; !ok {
		return fmt.Errorf("unknown provider %q (available: %v)", name, r.listUnlocked())
	}
	r.active = name
	return nil
}

func (r *Registry) Ollama() (*OllamaProvider, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.providers["ollama"].(*OllamaProvider)
	return p, ok && p != nil
}

func (r *Registry) List() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.listUnlocked()
}

func (r *Registry) listUnlocked() []string {
	out := make([]string, 0, len(r.providers))
	for k := range r.providers {
		out = append(out, k)
	}
	return out
}
