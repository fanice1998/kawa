package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/fanice1998/kawa/internal/config"
)

// GrokProvider implements both `xai_grok` (API Key) and `grok_oauth` (OAuth2 with auto-refresh).
type GrokProvider struct {
	name     string
	baseURL  string
	model    string
	apiKey   string
	oauthMgr *OAuthTokenManager
	client   *http.Client
}

func NewGrokAPIKeyProvider(cfg config.APIKeyModelConfig) *GrokProvider {
	timeout := time.Duration(cfg.TimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	return &GrokProvider{
		name:    "xai_grok",
		baseURL: strings.TrimRight(cfg.BaseURL, "/"),
		model:   cfg.Model,
		apiKey:  strings.TrimSpace(cfg.APIKey),
		client:  &http.Client{Timeout: timeout},
	}
}

func NewGrokOAuthProvider(cfg config.OAuthModelConfig) *GrokProvider {
	timeout := time.Duration(cfg.TimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	return &GrokProvider{
		name:     "grok_oauth",
		baseURL:  strings.TrimRight(cfg.BaseURL, "/"),
		model:    cfg.Model,
		oauthMgr: NewOAuthTokenManager("grok_oauth", cfg),
		client:   &http.Client{Timeout: timeout},
	}
}

func (p *GrokProvider) Name() string  { return p.name }
func (p *GrokProvider) Model() string { return p.model }

type openAIChatReq struct {
	Model    string          `json:"model"`
	Messages []openAIMessage `json:"messages"`
	Tools    []openAITool    `json:"tools,omitempty"`
}

type openAIMessage struct {
	Role       string           `json:"role"`
	Content    string           `json:"content,omitempty"`
	ToolCalls  []openAIToolCall `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
}

type openAITool struct {
	Type     string         `json:"type"`
	Function openAIFunction `json:"function"`
}

type openAIFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

type openAIToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type openAIChatResp struct {
	Choices []struct {
		Message openAIMessage `json:"message"`
	} `json:"choices"`
}

func (p *GrokProvider) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	msgs := make([]openAIMessage, 0, len(req.Messages))
	for _, m := range req.Messages {
		om := openAIMessage{
			Role:       string(m.Role),
			Content:    m.Content,
			ToolCallID: m.ToolCallID,
		}
		for _, tc := range m.ToolCalls {
			var call openAIToolCall
			call.ID = tc.ID
			call.Type = "function"
			call.Function.Name = tc.Name
			call.Function.Arguments = tc.Arguments
			om.ToolCalls = append(om.ToolCalls, call)
		}
		msgs = append(msgs, om)
	}

	var tools []openAITool
	for _, t := range req.Tools {
		tools = append(tools, openAITool{
			Type: "function",
			Function: openAIFunction{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.Parameters,
			},
		})
	}

	payload, err := json.Marshal(openAIChatReq{
		Model:    p.model,
		Messages: msgs,
		Tools:    tools,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal %s request: %w", p.name, err)
	}

	body, status, err := p.doRequest(ctx, payload, false)
	if err != nil {
		return nil, err
	}
	if status == http.StatusUnauthorized && p.oauthMgr != nil {
		// Auto-refresh OAuth token on 401 and retry once
		body, status, err = p.doRequest(ctx, payload, true)
		if err != nil {
			return nil, err
		}
	}

	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("%s HTTP %d: %s", p.name, status, string(body))
	}

	var oResp openAIChatResp
	if err := json.Unmarshal(body, &oResp); err != nil {
		return nil, fmt.Errorf("decode %s response: %w", p.name, err)
	}
	if len(oResp.Choices) == 0 {
		return nil, fmt.Errorf("%s returned empty choices", p.name)
	}

	choice := oResp.Choices[0].Message
	out := &ChatResponse{
		Content:  choice.Content,
		Provider: p.Name(),
		Model:    p.model,
	}
	for _, tc := range choice.ToolCalls {
		out.ToolCalls = append(out.ToolCalls, ToolCall{
			ID:        tc.ID,
			Name:      tc.Function.Name,
			Arguments: tc.Function.Arguments,
		})
	}
	return out, nil
}

func (p *GrokProvider) doRequest(ctx context.Context, payload []byte, forceRefresh bool) ([]byte, int, error) {
	token := p.apiKey
	if p.oauthMgr != nil {
		var err error
		if forceRefresh {
			token, err = p.oauthMgr.ForceRefresh(ctx)
		} else {
			token, err = p.oauthMgr.GetToken(ctx)
		}
		if err != nil {
			return nil, 0, err
		}
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return nil, 0, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+token)

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, 0, fmt.Errorf("%s request failed: %w", p.name, err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024))
	return body, resp.StatusCode, nil
}
