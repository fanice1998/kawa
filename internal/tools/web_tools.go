package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/fanice1998/kawa/internal/config"
)

var (
	scriptStyleRe = regexp.MustCompile(`(?is)<(script|style|noscript)[^>]*>.*?</(script|style|noscript)>`)
	htmlTagRe     = regexp.MustCompile(`(?s)<[^>]+>`)
	multiSpaceRe  = regexp.MustCompile(`[ \t]+`)
	multiLineRe   = regexp.MustCompile(`\n{3,}`)
	ddgResultRe   = regexp.MustCompile(`(?is)<a[^>]+class="[^"]*result__a[^"]*"[^>]+href="([^"]+)"[^>]*>(.*?)</a>`)
	ddgSnippetRe  = regexp.MustCompile(`(?is)<a[^>]+class="[^"]*result__snippet[^"]*"[^>]*>(.*?)</a>`)
)

// WebSearchTool searches the web for information using DuckDuckGo HTML or Brave Search API.
type WebSearchTool struct {
	cfg    config.WebToolConfig
	client *http.Client
}

func NewWebSearchTool(cfg config.WebToolConfig) *WebSearchTool {
	timeout := time.Duration(cfg.TimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	return &WebSearchTool{
		cfg:    cfg,
		client: &http.Client{Timeout: timeout},
	}
}

func (t *WebSearchTool) Name() string { return "web_search" }
func (t *WebSearchTool) Description() string {
	return "Search the internet for up-to-date documentation, news, packages, or technical references."
}
func (t *WebSearchTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"query": {
				"type": "string",
				"description": "Search query keywords."
			}
		},
		"required": ["query"]
	}`)
}

func (t *WebSearchTool) Execute(ctx context.Context, rawArgs string) (string, error) {
	var args struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal([]byte(rawArgs), &args); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	q := strings.TrimSpace(args.Query)
	if q == "" {
		return "", fmt.Errorf("query cannot be empty")
	}

	if strings.EqualFold(t.cfg.SearchProvider, "brave") && strings.TrimSpace(t.cfg.BraveAPIKey) != "" {
		return t.searchBrave(ctx, q)
	}
	return t.searchDuckDuckGo(ctx, q)
}

func (t *WebSearchTool) searchDuckDuckGo(ctx context.Context, query string) (string, error) {
	endpoint := "https://html.duckduckgo.com/html/?q=" + url.QueryEscape(query)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	ua := t.cfg.UserAgent
	if ua == "" {
		ua = "Mozilla/5.0 (Macintosh; Intel Mac OS X 14_0) AppleWebKit/537.36"
	}
	req.Header.Set("User-Agent", ua)

	resp, err := t.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("duckduckgo search request failed: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512*1024))
	page := string(body)

	links := ddgResultRe.FindAllStringSubmatch(page, 8)
	snippets := ddgSnippetRe.FindAllStringSubmatch(page, 8)

	if len(links) == 0 {
		return fmt.Sprintf("No search results found for %q.", query), nil
	}

	var sb strings.Builder
	for i, m := range links {
		rawHref := html.UnescapeString(m[1])
		// Unwrap DuckDuckGo redirect URL (?uddg=...)
		if u, err := url.Parse(rawHref); err == nil {
			if uddg := u.Query().Get("uddg"); uddg != "" {
				rawHref = uddg
			}
		}
		title := CleanHTMLToText(m[2])
		var snippet string
		if i < len(snippets) {
			snippet = CleanHTMLToText(snippets[i][1])
		}
		sb.WriteString(fmt.Sprintf("%d. %s\n   URL: %s\n", i+1, title, rawHref))
		if snippet != "" {
			sb.WriteString(fmt.Sprintf("   Snippet: %s\n", snippet))
		}
		sb.WriteString("\n")
	}
	return strings.TrimSpace(sb.String()), nil
}

func (t *WebSearchTool) searchBrave(ctx context.Context, query string) (string, error) {
	endpoint := "https://api.search.brave.com/res/v1/web/search?count=8&q=" + url.QueryEscape(query)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Subscription-Token", strings.TrimSpace(t.cfg.BraveAPIKey))

	resp, err := t.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("brave search failed: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512*1024))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("brave search HTTP %d: %s", resp.StatusCode, string(body))
	}

	var parsed struct {
		Web struct {
			Results []struct {
				Title       string `json:"title"`
				URL         string `json:"url"`
				Description string `json:"description"`
			} `json:"results"`
		} `json:"web"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", err
	}
	var sb strings.Builder
	for i, r := range parsed.Web.Results {
		sb.WriteString(fmt.Sprintf("%d. %s\n   URL: %s\n   Snippet: %s\n\n", i+1, r.Title, r.URL, CleanHTMLToText(r.Description)))
	}
	if sb.Len() == 0 {
		return "No search results found.", nil
	}
	return strings.TrimSpace(sb.String()), nil
}

// WebFetchTool fetches a URL and extracts clean readable text.
type WebFetchTool struct {
	cfg    config.WebToolConfig
	client *http.Client
}

func NewWebFetchTool(cfg config.WebToolConfig) *WebFetchTool {
	timeout := time.Duration(cfg.TimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	return &WebFetchTool{
		cfg:    cfg,
		client: &http.Client{Timeout: timeout},
	}
}

func (t *WebFetchTool) Name() string { return "web_fetch" }
func (t *WebFetchTool) Description() string {
	return "Fetch a web page by HTTP/HTTPS URL and extract its readable text content."
}
func (t *WebFetchTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"url": {
				"type": "string",
				"description": "Full HTTP/HTTPS URL to fetch."
			}
		},
		"required": ["url"]
	}`)
}

func (t *WebFetchTool) Execute(ctx context.Context, rawArgs string) (string, error) {
	var args struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal([]byte(rawArgs), &args); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	u, err := url.Parse(strings.TrimSpace(args.URL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return "", fmt.Errorf("only valid http:// or https:// URLs are allowed")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	ua := t.cfg.UserAgent
	if ua == "" {
		ua = "KawaAgent/1.0"
	}
	req.Header.Set("User-Agent", ua)

	resp, err := t.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch %s failed: %w", u.String(), err)
	}
	defer resp.Body.Close()

	maxBytes := t.cfg.MaxFetchBytes
	if maxBytes <= 0 {
		maxBytes = 204800
	}
	rawBytes, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes))
	if err != nil {
		return "", err
	}

	text := CleanHTMLToText(string(rawBytes))
	if len(text) > 16000 {
		text = text[:16000] + "\n...[truncated]"
	}
	return text, nil
}

// CleanHTMLToText removes script/style blocks and HTML tags, returning readable text.
func CleanHTMLToText(rawHTML string) string {
	s := scriptStyleRe.ReplaceAllString(rawHTML, " ")
	s = strings.ReplaceAll(s, "<br>", "\n")
	s = strings.ReplaceAll(s, "<br/>", "\n")
	s = strings.ReplaceAll(s, "</p>", "\n\n")
	s = strings.ReplaceAll(s, "</div>", "\n")
	s = strings.ReplaceAll(s, "</li>", "\n")
	s = htmlTagRe.ReplaceAllString(s, " ")
	s = html.UnescapeString(s)
	s = multiSpaceRe.ReplaceAllString(s, " ")
	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = strings.TrimSpace(lines[i])
	}
	s = strings.Join(lines, "\n")
	s = multiLineRe.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}
