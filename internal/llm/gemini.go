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

// GeminiProvider implements both `gemini` (API Key) and `gemini_oauth` (Google OAuth2 with auto-refresh).
type GeminiProvider struct {
	name      string
	baseURL   string
	model     string
	apiKey    string
	projectID string
	oauthMgr  *OAuthTokenManager
	client    *http.Client
}

func NewGeminiAPIKeyProvider(cfg config.APIKeyModelConfig) *GeminiProvider {
	timeout := time.Duration(cfg.TimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	return &GeminiProvider{
		name:    "gemini",
		baseURL: strings.TrimRight(cfg.BaseURL, "/"),
		model:   cfg.Model,
		apiKey:  strings.TrimSpace(cfg.APIKey),
		client:  &http.Client{Timeout: timeout},
	}
}

func NewGeminiOAuthProvider(cfg config.OAuthModelConfig) *GeminiProvider {
	timeout := time.Duration(cfg.TimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	return &GeminiProvider{
		name:      "gemini_oauth",
		baseURL:   strings.TrimRight(cfg.BaseURL, "/"),
		model:     cfg.Model,
		projectID: strings.TrimSpace(cfg.ProjectID),
		oauthMgr:  NewOAuthTokenManager("gemini_oauth", cfg),
		client:    &http.Client{Timeout: timeout},
	}
}

func (p *GeminiProvider) Name() string  { return p.name }
func (p *GeminiProvider) Model() string { return p.model }

type geminiReq struct {
	SystemInstruction *geminiContent     `json:"systemInstruction,omitempty"`
	Contents          []geminiContent    `json:"contents"`
	Tools             []geminiToolBundle `json:"tools,omitempty"`
}

type geminiContent struct {
	Role  string       `json:"role,omitempty"`
	Parts []geminiPart `json:"parts"`
}

type geminiPart struct {
	Text             string              `json:"text,omitempty"`
	FunctionCall     *geminiFunctionCall `json:"functionCall,omitempty"`
	FunctionResponse *geminiFuncResp     `json:"functionResponse,omitempty"`
}

type geminiFunctionCall struct {
	Name string          `json:"name"`
	Args json.RawMessage `json:"args"`
}

type geminiFuncResp struct {
	Name     string         `json:"name"`
	Response map[string]any `json:"response"`
}

type geminiToolBundle struct {
	FunctionDeclarations []geminiFuncDecl `json:"functionDeclarations"`
}

type geminiFuncDecl struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type geminiResp struct {
	Candidates []struct {
		Content geminiContent `json:"content"`
	} `json:"candidates"`
}

func (p *GeminiProvider) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	var sysParts []geminiPart
	var contents []geminiContent

	for _, m := range req.Messages {
		switch m.Role {
		case RoleSystem:
			if strings.TrimSpace(m.Content) != "" {
				sysParts = append(sysParts, geminiPart{Text: m.Content})
			}
		case RoleUser:
			contents = append(contents, geminiContent{
				Role:  "user",
				Parts: []geminiPart{{Text: m.Content}},
			})
		case RoleAssistant:
			var parts []geminiPart
			if strings.TrimSpace(m.Content) != "" {
				parts = append(parts, geminiPart{Text: m.Content})
			}
			for _, tc := range m.ToolCalls {
				rawArgs := json.RawMessage(tc.Arguments)
				if !json.Valid(rawArgs) {
					rawArgs = json.RawMessage("{}")
				}
				parts = append(parts, geminiPart{
					FunctionCall: &geminiFunctionCall{
						Name: tc.Name,
						Args: rawArgs,
					},
				})
			}
			if len(parts) > 0 {
				contents = append(contents, geminiContent{
					Role:  "model",
					Parts: parts,
				})
			}
		case RoleTool:
			toolName := m.ToolName
			if toolName == "" {
				toolName = "tool"
			}
			contents = append(contents, geminiContent{
				Role: "user",
				Parts: []geminiPart{
					{
						FunctionResponse: &geminiFuncResp{
							Name:     toolName,
							Response: map[string]any{"output": m.Content},
						},
					},
				},
			})
		}
	}

	gReq := geminiReq{
		Contents: contents,
	}
	if len(sysParts) > 0 {
		gReq.SystemInstruction = &geminiContent{Parts: sysParts}
	}
	if len(req.Tools) > 0 {
		decls := make([]geminiFuncDecl, 0, len(req.Tools))
		for _, t := range req.Tools {
			decls = append(decls, geminiFuncDecl{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.Parameters,
			})
		}
		gReq.Tools = []geminiToolBundle{{FunctionDeclarations: decls}}
	}

	payload, err := json.Marshal(gReq)
	if err != nil {
		return nil, fmt.Errorf("marshal %s request: %w", p.name, err)
	}

	body, status, err := p.doRequest(ctx, payload, false)
	if err != nil {
		return nil, err
	}
	if status == http.StatusUnauthorized && p.oauthMgr != nil {
		body, status, err = p.doRequest(ctx, payload, true)
		if err != nil {
			return nil, err
		}
	}

	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("%s HTTP %d: %s", p.name, status, string(body))
	}

	var gResp geminiResp
	if err := json.Unmarshal(body, &gResp); err != nil {
		return nil, fmt.Errorf("decode %s response: %w", p.name, err)
	}
	if len(gResp.Candidates) == 0 {
		return nil, fmt.Errorf("%s returned empty candidates", p.name)
	}

	out := &ChatResponse{
		Provider: p.Name(),
		Model:    p.model,
	}
	var textParts []string
	for i, part := range gResp.Candidates[0].Content.Parts {
		if part.Text != "" {
			textParts = append(textParts, part.Text)
		}
		if part.FunctionCall != nil {
			out.ToolCalls = append(out.ToolCalls, ToolCall{
				ID:        fmt.Sprintf("gemini-call-%d", i+1),
				Name:      part.FunctionCall.Name,
				Arguments: string(part.FunctionCall.Args),
			})
		}
	}
	out.Content = strings.Join(textParts, "\n")
	return out, nil
}

func (p *GeminiProvider) doRequest(ctx context.Context, payload []byte, forceRefresh bool) ([]byte, int, error) {
	endpoint := fmt.Sprintf("%s/models/%s:generateContent", p.baseURL, p.model)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, 0, err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	if p.oauthMgr != nil {
		var tok string
		if forceRefresh {
			tok, err = p.oauthMgr.ForceRefresh(ctx)
		} else {
			tok, err = p.oauthMgr.GetToken(ctx)
		}
		if err != nil {
			return nil, 0, err
		}
		httpReq.Header.Set("Authorization", "Bearer "+tok)
		if p.projectID != "" {
			httpReq.Header.Set("x-goog-user-project", p.projectID)
		}
	} else {
		httpReq.Header.Set("x-goog-api-key", p.apiKey)
	}

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, 0, fmt.Errorf("%s request failed: %w", p.name, err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024))
	return body, resp.StatusCode, nil
}
