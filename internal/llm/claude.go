package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/fanice1998/kawa/internal/config"
)

type ClaudeProvider struct {
	mu        sync.RWMutex
	baseURL   string
	apiKey    string
	model     string
	maxTokens int
	version   string
	client    *http.Client
}

func NewClaudeProvider(cfg config.ClaudeConfig) *ClaudeProvider {
	timeout := time.Duration(cfg.TimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	maxTokens := cfg.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 8192
	}
	ver := cfg.AnthropicVersion
	if ver == "" {
		ver = "2023-06-01"
	}
	return &ClaudeProvider{
		baseURL:   strings.TrimRight(cfg.BaseURL, "/"),
		apiKey:    strings.TrimSpace(cfg.APIKey),
		model:     cfg.Model,
		maxTokens: maxTokens,
		version:   ver,
		client:    &http.Client{Timeout: timeout},
	}
}

func (p *ClaudeProvider) Name() string { return "claude" }

func (p *ClaudeProvider) Model() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.model
}

func (p *ClaudeProvider) SetModel(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("model name is empty")
	}
	p.mu.Lock()
	p.model = name
	p.mu.Unlock()
	return nil
}

// ListModels calls GET {baseURL}/models and follows after_id pages.
func (p *ClaudeProvider) ListModels(ctx context.Context) ([]ListedModel, error) {
	var all []ListedModel
	after := ""
	for page := 0; page < 5; page++ {
		q := url.Values{}
		q.Set("limit", "100")
		if after != "" {
			q.Set("after_id", after)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.baseURL+"/models?"+q.Encode(), nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("x-api-key", p.apiKey)
		req.Header.Set("anthropic-version", p.version)
		req.Header.Set("Accept", "application/json")

		resp, err := p.client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("claude list models: %w", err)
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024))
		resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return nil, fmt.Errorf("claude list models HTTP %d: %s", resp.StatusCode, clipBody(body))
		}
		env, models, err := decodeModelPage(body, false)
		if err != nil {
			return nil, err
		}
		all = append(all, models...)
		next := strings.TrimSpace(env.LastID)
		if next == "" && len(env.Data) > 0 {
			next = env.Data[len(env.Data)-1].listedID()
		}
		if !env.HasMore || next == "" || next == after {
			break
		}
		after = next
	}
	return dedupeListed(all), nil
}

type claudeReq struct {
	Model     string          `json:"model"`
	MaxTokens int             `json:"max_tokens"`
	System    string          `json:"system,omitempty"`
	Messages  []claudeMessage `json:"messages"`
	Tools     []claudeTool    `json:"tools,omitempty"`
}

type claudeMessage struct {
	Role    string        `json:"role"`
	Content []claudeBlock `json:"content"`
}

type claudeBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   string          `json:"content,omitempty"`
}

type claudeTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type claudeResp struct {
	Content []claudeBlock `json:"content"`
}

func (p *ClaudeProvider) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	model := p.Model()
	var systemParts []string
	var msgs []claudeMessage

	for _, m := range req.Messages {
		switch m.Role {
		case RoleSystem:
			if strings.TrimSpace(m.Content) != "" {
				systemParts = append(systemParts, m.Content)
			}
		case RoleUser:
			msgs = append(msgs, claudeMessage{
				Role: "user",
				Content: []claudeBlock{
					{Type: "text", Text: m.Content},
				},
			})
		case RoleAssistant:
			var blocks []claudeBlock
			if strings.TrimSpace(m.Content) != "" {
				blocks = append(blocks, claudeBlock{Type: "text", Text: m.Content})
			}
			for _, tc := range m.ToolCalls {
				rawInput := json.RawMessage(tc.Arguments)
				if !json.Valid(rawInput) {
					rawInput = json.RawMessage("{}")
				}
				blocks = append(blocks, claudeBlock{
					Type:  "tool_use",
					ID:    tc.ID,
					Name:  tc.Name,
					Input: rawInput,
				})
			}
			if len(blocks) > 0 {
				msgs = append(msgs, claudeMessage{Role: "assistant", Content: blocks})
			}
		case RoleTool:
			msgs = append(msgs, claudeMessage{
				Role: "user",
				Content: []claudeBlock{
					{
						Type:      "tool_result",
						ToolUseID: m.ToolCallID,
						Content:   m.Content,
					},
				},
			})
		}
	}

	var tools []claudeTool
	for _, t := range req.Tools {
		tools = append(tools, claudeTool{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: t.Parameters,
		})
	}

	payload, err := json.Marshal(claudeReq{
		Model:     model,
		MaxTokens: p.maxTokens,
		System:    strings.Join(systemParts, "\n\n"),
		Messages:  msgs,
		Tools:     tools,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal claude request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/messages", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", p.apiKey)
	httpReq.Header.Set("anthropic-version", p.version)

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("claude request failed: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("claude HTTP %d: %s", resp.StatusCode, string(body))
	}

	var cResp claudeResp
	if err := json.Unmarshal(body, &cResp); err != nil {
		return nil, fmt.Errorf("decode claude response: %w", err)
	}

	out := &ChatResponse{
		Provider: p.Name(),
		Model:    model,
	}
	var textParts []string
	for _, b := range cResp.Content {
		switch b.Type {
		case "text":
			textParts = append(textParts, b.Text)
		case "tool_use":
			out.ToolCalls = append(out.ToolCalls, ToolCall{
				ID:        b.ID,
				Name:      b.Name,
				Arguments: string(b.Input),
			})
		}
	}
	out.Content = strings.Join(textParts, "\n")
	return out, nil
}
