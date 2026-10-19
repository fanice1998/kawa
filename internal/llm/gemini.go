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

// GeminiProvider implements both `gemini` (API Key) and `gemini_oauth` (Google OAuth2 with auto-refresh).
type GeminiProvider struct {
	mu        sync.RWMutex
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

func (p *GeminiProvider) Name() string { return p.name }

func (p *GeminiProvider) Model() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.model
}

func (p *GeminiProvider) SetModel(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("model name is empty")
	}
	p.mu.Lock()
	p.model = name
	p.mu.Unlock()
	return nil
}

// ListModels calls GET {baseURL}/models and keeps models that support generateContent.
func (p *GeminiProvider) ListModels(ctx context.Context) ([]ListedModel, error) {
	var all []ListedModel
	pageToken := ""
	for page := 0; page < 5; page++ {
		q := url.Values{}
		q.Set("pageSize", "100")
		if pageToken != "" {
			q.Set("pageToken", pageToken)
		}
		endpoint := p.baseURL + "/models?" + q.Encode()
		body, status, err := p.authorizedGET(ctx, endpoint, false)
		if err != nil {
			return nil, err
		}
		if status == http.StatusUnauthorized && p.oauthMgr != nil {
			body, status, err = p.authorizedGET(ctx, endpoint, true)
			if err != nil {
				return nil, err
			}
		}
		if status < 200 || status >= 300 {
			return nil, fmt.Errorf("%s list models HTTP %d: %s", p.name, status, clipBody(body))
		}
		env, models, err := decodeModelPage(body, true)
		if err != nil {
			return nil, err
		}
		all = append(all, models...)
		next := strings.TrimSpace(env.NextPageToken)
		if next == "" || next == pageToken {
			break
		}
		pageToken = next
	}
	return dedupeListed(all), nil
}

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
	model := p.Model()
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

	body, status, err := p.doRequest(ctx, model, payload, false)
	if err != nil {
		return nil, err
	}
	if status == http.StatusUnauthorized && p.oauthMgr != nil {
		body, status, err = p.doRequest(ctx, model, payload, true)
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
		Model:    model,
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

func (p *GeminiProvider) authorize(ctx context.Context, req *http.Request, forceRefresh bool) error {
	if p.oauthMgr != nil {
		var tok string
		var err error
		if forceRefresh {
			tok, err = p.oauthMgr.ForceRefresh(ctx)
		} else {
			tok, err = p.oauthMgr.GetToken(ctx)
		}
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		if p.projectID != "" {
			req.Header.Set("x-goog-user-project", p.projectID)
		}
		return nil
	}
	req.Header.Set("x-goog-api-key", p.apiKey)
	return nil
}

func (p *GeminiProvider) authorizedGET(ctx context.Context, endpoint string, forceRefresh bool) ([]byte, int, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, 0, err
	}
	httpReq.Header.Set("Accept", "application/json")
	if err := p.authorize(ctx, httpReq, forceRefresh); err != nil {
		return nil, 0, err
	}
	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, 0, fmt.Errorf("%s list models: %w", p.name, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024))
	return body, resp.StatusCode, nil
}

func (p *GeminiProvider) doRequest(ctx context.Context, model string, payload []byte, forceRefresh bool) ([]byte, int, error) {
	endpoint := fmt.Sprintf("%s/models/%s:generateContent", p.baseURL, model)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, 0, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if err := p.authorize(ctx, httpReq, forceRefresh); err != nil {
		return nil, 0, err
	}

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, 0, fmt.Errorf("%s request failed: %w", p.name, err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024))
	return body, resp.StatusCode, nil
}
