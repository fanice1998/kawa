package channel

import (
	"strings"
	"testing"
)

func TestFormatTelegramHTMLRendersMarkdown(t *testing.T) {
	got := formatTelegramHTML("這是 **粗體**、*斜體* 與 `code`，路徑 internal/tools/foo_bar.go")
	if !strings.Contains(got, "<b>粗體</b>") || !strings.Contains(got, "<i>斜體</i>") || !strings.Contains(got, "<code>code</code>") {
		t.Fatalf("formatting was stripped:\n%s", got)
	}
	if strings.Contains(got, `\*`) || strings.Contains(got, "<i>tools") {
		t.Fatalf("path or markers were mangled:\n%s", got)
	}
	if !strings.Contains(got, "foo_bar.go") {
		t.Fatalf("file path missing:\n%s", got)
	}

	link := formatTelegramHTML("看 [說明](https://example.com/a?b=1&c=2)")
	if !strings.Contains(link, `href="https://example.com/a?b=1&amp;c=2"`) || !strings.Contains(link, ">說明</a>") {
		t.Fatalf("link not rendered:\n%s", link)
	}

	fence := formatTelegramHTML("```go\nfmt.Println(\"<hi>\")\n```")
	if !strings.Contains(fence, "<pre>") || !strings.Contains(fence, "&lt;hi&gt;") || strings.Contains(fence, "<hi>") {
		t.Fatalf("code fence not rendered:\n%s", fence)
	}

	if plain := formatTelegramHTML("a < b"); plain != "a &lt; b" {
		t.Fatalf("html was not escaped: %q", plain)
	}
}

func TestTelegramEntityError(t *testing.T) {
	if !telegramEntityError(errString("telegram sendMessage HTTP 400: can't parse entities")) {
		t.Fatal("expected parse error to be retryable as plain text")
	}
	if telegramEntityError(errString("telegram sendMessage: dial tcp: connection refused")) {
		t.Fatal("network errors must not be treated as entity errors")
	}
}

type errString string

func (e errString) Error() string { return string(e) }
