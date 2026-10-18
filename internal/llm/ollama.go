package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/fanice1998/kawa/internal/config"
)

var (
	toolCallTagRe = regexp.MustCompile(`(?is)<tool_call>\s*(\{.*?\})\s*</tool_call>`)
	jsonCodeFence = regexp.MustCompile("(?is)```(?:json)?\\s*(\\{.*?\\})\\s*```")
)

type OllamaProvider struct {
	mu          sync.RWMutex
	baseURL     string
	model       string
	idleTimeout time.Duration
	client      *http.Client
}

func NewOllamaProvider(cfg config.OllamaConfig) *OllamaProvider {
	idleTimeout := time.Duration(cfg.TimeoutSec) * time.Second
	if idleTimeout < 180*time.Second {
		idleTimeout = 180 * time.Second
	}
	return &OllamaProvider{
		baseURL:     strings.TrimRight(cfg.BaseURL, "/"),
		model:       cfg.Model,
		idleTimeout: idleTimeout,
		// Do NOT set a hard wall-clock Timeout on http.Client; use per-chunk idle watchdog instead
		// so actively streaming responses from 27B+ models are never cut off mid-stream.
		client: &http.Client{
			Transport: &http.Transport{
				ResponseHeaderTimeout: idleTimeout,
				IdleConnTimeout:       90 * time.Second,
			},
		},
	}
}

func (p *OllamaProvider) Name() string { return "ollama" }

func (p *OllamaProvider) Model() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.model
}

func (p *OllamaProvider) SetModel(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("model name is empty")
	}
	p.mu.Lock()
	p.model = name
	p.mu.Unlock()
	return nil
}

type ollamaTagsResp struct {
	Models []struct {
		Name  string `json:"name"`
		Model string `json:"model"`
	} `json:"models"`
}

// ListModels returns model names from the local Ollama daemon (`GET /api/tags`).
func (p *OllamaProvider) ListModels(ctx context.Context) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.baseURL+"/api/tags", nil)
	if err != nil {
		return nil, err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ollama list models: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("ollama list models HTTP %d: %s", resp.StatusCode, string(body))
	}
	var parsed ollamaTagsResp
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("decode ollama tags: %w", err)
	}
	var names []string
	seen := map[string]bool{}
	for _, m := range parsed.Models {
		name := strings.TrimSpace(m.Name)
		if name == "" {
			name = strings.TrimSpace(m.Model)
		}
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}
	return names, nil
}

// ResolveOllamaModel picks an installed model. A name without a tag matches
// when exactly one installed model uses that base name.
func ResolveOllamaModel(want string, available []string) (string, error) {
	want = strings.TrimSpace(want)
	if want == "" {
		return "", fmt.Errorf("模型名稱是空的")
	}
	if len(available) == 0 {
		return want, nil
	}
	for _, name := range available {
		if name == want {
			return name, nil
		}
	}
	var matched []string
	for _, name := range available {
		base := name
		if i := strings.Index(name, ":"); i >= 0 {
			base = name[:i]
		}
		if base == want {
			matched = append(matched, name)
		}
	}
	if len(matched) == 1 {
		return matched[0], nil
	}
	return "", fmt.Errorf("Ollama 沒有模型 %q。可用：%s", want, strings.Join(available, ", "))
}

type ollamaChatReq struct {
	Model    string          `json:"model"`
	Messages []ollamaMessage `json:"messages"`
	Tools    []ollamaTool    `json:"tools,omitempty"`
	Stream   bool            `json:"stream"`
	Think    *bool           `json:"think,omitempty"`
	Options  map[string]any  `json:"options,omitempty"`
}

type ollamaMessage struct {
	Role      string           `json:"role"`
	Content   string           `json:"content"`
	Thinking  string           `json:"thinking,omitempty"`
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
	Done    bool          `json:"done"`
}

// activityReader resets the watchdog timer every time bytes arrive from the Ollama stream,
// ensuring long responses never fail with "context deadline exceeded" as long as tokens are flowing.
type activityReader struct {
	r     io.Reader
	timer *time.Timer
	idle  time.Duration
}

func (ar *activityReader) Read(p []byte) (int, error) {
	n, err := ar.r.Read(p)
	if n > 0 && ar.timer != nil {
		ar.timer.Reset(ar.idle)
	}
	return n, err
}

func (p *OllamaProvider) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	// Pre-flight inspection & sanitization before sending to Ollama
	req, _ = SanitizeAndValidateRequest(req)

	msgs := make([]ollamaMessage, 0, len(req.Messages))
	for _, m := range req.Messages {
		om := ollamaMessage{
			Role:    string(m.Role),
			Content: m.Content,
		}
		if m.Role == RoleTool {
			// Qwen2.5-Coder, Qwen3 & Hermes process tool results fastest when Role is "tool" with clean content
			if m.ToolName != "" && !strings.HasPrefix(strings.TrimSpace(om.Content), "<tool_response>") {
				om.Content = fmt.Sprintf("<tool_response>\nTool `%s` output:\n%s\n</tool_response>\n請根據以上工具執行結果，直接用繁體中文回答使用者的問題，不要再輸出 JSON。", m.ToolName, m.Content)
			}
		} else if len(m.ToolCalls) > 0 {
			for _, tc := range m.ToolCalls {
				rawArgs := strings.TrimSpace(tc.Arguments)
				if rawArgs == "" || !json.Valid([]byte(rawArgs)) {
					rawArgs = "{}"
				}
				var oc ollamaToolCall
				oc.Function.Name = tc.Name
				oc.Function.Arguments = json.RawMessage(rawArgs)
				om.ToolCalls = append(om.ToolCalls, oc)
			}
			if strings.TrimSpace(om.Content) == "" && len(req.Tools) == 0 {
				var sb strings.Builder
				for _, tc := range m.ToolCalls {
					rawArgs := strings.TrimSpace(tc.Arguments)
					if rawArgs == "" {
						rawArgs = "{}"
					}
					sb.WriteString(fmt.Sprintf("<tool_call>\n{\"name\": %q, \"arguments\": %s}\n</tool_call>\n", tc.Name, rawArgs))
				}
				om.Content = strings.TrimSpace(sb.String())
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

	// Disable native Qwen3/Qwen3.5/Qwen3.6 thinking chain ("think": false) so 27B models
	// output responses, tool calls, and code patches immediately without spending 3-5 minutes on hidden thinking tokens.
	thinkFalse := false

	model := p.Model()
	payload, err := json.Marshal(ollamaChatReq{
		Model:    model,
		Messages: msgs,
		Tools:    tools,
		Stream:   true,
		Think:    &thinkFalse,
		Options: map[string]any{
			"num_ctx":     8192,
			"num_predict": 4096,
			"temperature": 0.2,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("marshal ollama request: %w", err)
	}

	// Create a stream context that cancels ONLY if the parent ctx is cancelled OR if no bytes arrive for p.idleTimeout
	streamCtx, cancelStream := context.WithCancel(ctx)
	idleTimer := time.AfterFunc(p.idleTimeout, func() {
		cancelStream()
	})
	defer func() {
		idleTimer.Stop()
		cancelStream()
	}()

	httpReq, err := http.NewRequestWithContext(streamCtx, http.MethodPost, p.baseURL+"/api/chat", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("ollama request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 256*1024))
		return nil, fmt.Errorf("ollama HTTP %d: %s", resp.StatusCode, string(body))
	}

	var contentBuilder strings.Builder
	var rawToolCalls []ollamaToolCall

	watchedBody := &activityReader{
		r:     io.LimitReader(resp.Body, 8*1024*1024),
		timer: idleTimer,
		idle:  p.idleTimeout,
	}
	dec := json.NewDecoder(watchedBody)
	for {
		var chunk ollamaChatResp
		if err := dec.Decode(&chunk); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			// If we already received meaningful content or tool calls before a transient stream close, recover gracefully!
			if contentBuilder.Len() > 0 || len(rawToolCalls) > 0 {
				break
			}
			return nil, fmt.Errorf("decode ollama response chunk: %w", err)
		}
		contentBuilder.WriteString(chunk.Message.Content)
		if len(chunk.Message.ToolCalls) > 0 {
			rawToolCalls = append(rawToolCalls, chunk.Message.ToolCalls...)
		}
	}

	cleanContent := StripThinkingTags(contentBuilder.String())
	out := &ChatResponse{
		Content:  cleanContent,
		Provider: p.Name(),
		Model:    model,
	}
	for i, tc := range rawToolCalls {
		out.ToolCalls = append(out.ToolCalls, ToolCall{
			ID:        fmt.Sprintf("ollama-call-%d", i+1),
			Name:      tc.Function.Name,
			Arguments: string(tc.Function.Arguments),
		})
	}

	// Extract Hermes `<tool_call>{...}</tool_call>`, fenced JSON, or embedded JSON tool calls anywhere in content
	if len(out.ToolCalls) == 0 && len(validTools) > 0 && strings.TrimSpace(out.Content) != "" {
		if cleanText, inlineCalls := extractInlineToolCalls(out.Content, validTools); len(inlineCalls) > 0 {
			out.Content = cleanText
			out.ToolCalls = inlineCalls
		}
	}

	return out, nil
}

// extractInlineToolCalls detects Hermes `<tool_call>` tags, fenced JSON, or any balanced JSON object
// inside `content` that references a valid tool name.
func extractInlineToolCalls(content string, validTools map[string]bool) (string, []ToolCall) {
	trimmed := strings.TrimSpace(content)

	// 1. Check Hermes `<tool_call>{...}</tool_call>`
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

	// 3. Scan for any balanced JSON object `{...}` inside the text (handles mixed text + JSON)
	candidates := extractBalancedJSONObjects(trimmed)
	var calls []ToolCall
	clean := trimmed
	for i, cand := range candidates {
		if tc, ok := parseSingleToolJSON(cand, i+1, validTools); ok {
			calls = append(calls, tc)
			clean = strings.TrimSpace(strings.Replace(clean, cand, "", 1))
		}
	}
	if len(calls) > 0 {
		return clean, calls
	}

	return content, nil
}

// extractBalancedJSONObjects finds top-level `{...}` substrings in s, respecting string quotes and escapes.
func extractBalancedJSONObjects(s string) []string {
	var results []string
	depth := 0
	start := -1
	inString := false
	escaped := false

	for i := 0; i < len(s); i++ {
		ch := s[i]
		if escaped {
			escaped = false
			continue
		}
		if ch == '\\' && inString {
			escaped = true
			continue
		}
		if ch == '"' {
			inString = !inString
			continue
		}
		if inString {
			continue
		}
		if ch == '{' {
			if depth == 0 {
				start = i
			}
			depth++
		} else if ch == '}' {
			if depth > 0 {
				depth--
				if depth == 0 && start != -1 {
					results = append(results, s[start:i+1])
					start = -1
				}
			}
		}
	}
	return results
}

func parseSingleToolJSON(raw string, idx int, validTools map[string]bool) (ToolCall, bool) {
	var parsed struct {
		Name       string          `json:"name"`
		Tool       string          `json:"tool"`
		Function   string          `json:"function"`
		Arguments  json.RawMessage `json:"arguments"`
		Parameters json.RawMessage `json:"parameters"`
		Args       json.RawMessage `json:"args"`
	}
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return ToolCall{}, false
	}
	name := strings.TrimSpace(parsed.Name)
	if name == "" {
		name = strings.TrimSpace(parsed.Tool)
	}
	if name == "" {
		name = strings.TrimSpace(parsed.Function)
	}
	if name == "" || !validTools[name] {
		return ToolCall{}, false
	}
	args := parsed.Arguments
	if len(args) == 0 {
		args = parsed.Parameters
	}
	if len(args) == 0 {
		args = parsed.Args
	}
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
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
