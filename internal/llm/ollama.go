package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/fanice1998/kawa/internal/config"
)

var (
	toolCallTagRe  = regexp.MustCompile(`(?is)<tool_call>\s*(\{.*?\})\s*</tool_call>`)
	jsonCodeFence  = regexp.MustCompile("(?is)```(?:json)?\\s*(\\{.*?\\})\\s*```")
)

type OllamaProvider struct {
	baseURL string
	model   string
	client  *http.Client
}

func NewOllamaProvider(cfg config.OllamaConfig) *OllamaProvider {
	timeout := time.Duration(cfg.TimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	return &OllamaProvider{
		baseURL: strings.TrimRight(cfg.BaseURL, "/"),
		model:   cfg.Model,
		client:  &http.Client{Timeout: timeout},
	}
}

func (p *OllamaProvider) Name() string  { return "ollama" }
func (p *OllamaProvider) Model() string { return p.model }

type ollamaChatReq struct {
	Model    string          `json:"model"`
	Messages []ollamaMessage `json:"messages"`
	Tools    []ollamaTool    `json:"tools,omitempty"`
	Stream   bool            `json:"stream"`
}

type ollamaMessage struct {
	Role      string           `json:"role"`
	Content   string           `json:"content"`
	ToolCalls []ollamaToolCall `json:"tool_calls,omitempty"`
}

type ollamaTool struct {
	Type     string         `json:"type"`
	Function ollamaFunction `json:"function"`
}

type ollamaFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

type ollamaToolCall struct {
	Function struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"function"`
}

type ollamaChatResp struct {
	Message ollamaMessage `json:"message"`
}

func (p *OllamaProvider) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	msgs := make([]ollamaMessage, 0, len(req.Messages))
	for _, m := range req.Messages {
		om := ollamaMessage{
			Role:    string(m.Role),
			Content: m.Content,
		}
		for _, tc := range m.ToolCalls {
			rawArgs := json.RawMessage(tc.Arguments)
			if !json.Valid(rawArgs) {
				rawArgs = json.RawMessage("{}")
			}
			var call ollamaToolCall
			call.Function.Name = tc.Name
			call.Function.Arguments = rawArgs
			om.ToolCalls = append(om.ToolCalls, call)
			// Many Ollama models (like qwen2.5-coder) expect their own prior tool call in Content when re-reading history
			if strings.TrimSpace(om.Content) == "" {
				om.Content = fmt.Sprintf(`{"name": %q, "arguments": %s}`, tc.Name, string(rawArgs))
			}
		}
		msgs = append(msgs, om)
	}

	validTools := make(map[string]bool, len(req.Tools))
	var tools []ollamaTool
	for _, t := range req.Tools {
		validTools[t.Name] = true
		tools = append(tools, ollamaTool{
			Type: "function",
			Function: ollamaFunction{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.Parameters,
			},
		})
	}

	payload, err := json.Marshal(ollamaChatReq{
		Model:    p.model,
		Messages: msgs,
		Tools:    tools,
		Stream:   false,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal ollama request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/api/chat", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("ollama request failed: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("ollama HTTP %d: %s", resp.StatusCode, string(body))
	}

	var oResp ollamaChatResp
	if err := json.Unmarshal(body, &oResp); err != nil {
		return nil, fmt.Errorf("decode ollama response: %w", err)
	}

	out := &ChatResponse{
		Content:  oResp.Message.Content,
		Provider: p.Name(),
		Model:    p.model,
	}
	for i, tc := range oResp.Message.ToolCalls {
		out.ToolCalls = append(out.ToolCalls, ToolCall{
			ID:        fmt.Sprintf("ollama-call-%d", i+1),
			Name:      tc.Function.Name,
			Arguments: string(tc.Function.Arguments),
		})
	}

	// Fallback: models like qwen2.5-coder often emit `{"name": "tool", "arguments": {...}}`
	// directly inside `message.content` instead of `message.tool_calls`.
	if len(out.ToolCalls) == 0 && len(validTools) > 0 && strings.TrimSpace(out.Content) != "" {
		if cleanText, inlineCalls := extractInlineToolCalls(out.Content, validTools); len(inlineCalls) > 0 {
			out.Content = cleanText
			out.ToolCalls = inlineCalls
		}
	}

	return out, nil
}

// extractInlineToolCalls detects JSON tool calls emitted inside `message.content` by models like qwen2.5-coder.
func extractInlineToolCalls(content string, validTools map[string]bool) (string, []ToolCall) {
	trimmed := strings.TrimSpace(content)

	// 1. Check `<tool_call>{...}</tool_call>`
	if matches := toolCallTagRe.FindAllStringSubmatch(trimmed, -1); len(matches) > 0 {
		var calls []ToolCall
		for i, m := range matches {
			if tc, ok := parseSingleToolJSON(m[1], i+1, validTools); ok {
				calls = append(calls, tc)
			}
		}
		if len(calls) > 0 {
			clean := strings.TrimSpace(toolCallTagRe.ReplaceAllString(trimmed, ""))
			return clean, calls
		}
	}

	// 2. Check ```json {...} ```
	if matches := jsonCodeFence.FindAllStringSubmatch(trimmed, -1); len(matches) > 0 {
		var calls []ToolCall
		for i, m := range matches {
			if tc, ok := parseSingleToolJSON(m[1], i+1, validTools); ok {
				calls = append(calls, tc)
			}
		}
		if len(calls) > 0 {
			clean := strings.TrimSpace(jsonCodeFence.ReplaceAllString(trimmed, ""))
			return clean, calls
		}
	}

	// 3. Check raw JSON object `{"name": "...", "arguments": {...}}`
	if strings.HasPrefix(trimmed, "{") && strings.HasSuffix(trimmed, "}") {
		if tc, ok := parseSingleToolJSON(trimmed, 1, validTools); ok {
			return "", []ToolCall{tc}
		}
	}

	return content, nil
}

func parseSingleToolJSON(raw string, idx int, validTools map[string]bool) (ToolCall, bool) {
	var parsed struct {
		Name       string          `json:"name"`
		Arguments  json.RawMessage `json:"arguments"`
		Parameters json.RawMessage `json:"parameters"`
	}
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return ToolCall{}, false
	}
	name := strings.TrimSpace(parsed.Name)
	if name == "" || !validTools[name] {
		return ToolCall{}, false
	}
	args := parsed.Arguments
	if len(args) == 0 {
		args = parsed.Parameters
	}
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	// If arguments was encoded as a JSON string inside JSON, unwrap it
	if len(args) > 0 && args[0] == '"' {
		var unquoted string
		if err := json.Unmarshal(args, &unquoted); err == nil && json.Valid([]byte(unquoted)) {
			args = json.RawMessage(unquoted)
		}
	}
	return ToolCall{
		ID:        fmt.Sprintf("ollama-inline-%d", idx),
		Name:      name,
		Arguments: string(args),
	}, true
}
