package channel

import (
	"fmt"
	"html"
	"regexp"
	"strings"
)

var (
	telegramFenceRe  = regexp.MustCompile("(?s)```(?:[A-Za-z0-9_+-]*)?[ \\t]*\\n?(.*?)```")
	telegramInlineRe = regexp.MustCompile("`([^`\\n]+)`")
	telegramLinkRe   = regexp.MustCompile(`\[([^\]\n]+)\]\((https?://[^\s)]+)\)`)
	telegramBoldRe   = regexp.MustCompile(`\*\*([^*\n]+)\*\*|__([^_\n]+)__`)
	telegramItalicRe = regexp.MustCompile(`\*([^*\n]+)\*`)
	telegramStrikeRe = regexp.MustCompile(`~~([^~\n]+)~~`)
)

// formatTelegramHTML converts a small markdown subset into Telegram HTML.
// Characters that are not part of that subset are escaped, so file names
// with underscores stay literal and a parse failure can fall back to plain text.
func formatTelegramHTML(src string) string {
	var slots []string
	protect := func(fragment string) string {
		slots = append(slots, fragment)
		return fmt.Sprintf("\x00K%d\x00", len(slots)-1)
	}

	s := telegramFenceRe.ReplaceAllStringFunc(src, func(m string) string {
		body := ""
		if sub := telegramFenceRe.FindStringSubmatch(m); len(sub) > 1 {
			body = strings.Trim(sub[1], "\n")
		}
		return protect("<pre>" + html.EscapeString(body) + "</pre>")
	})
	s = telegramInlineRe.ReplaceAllStringFunc(s, func(m string) string {
		body := ""
		if sub := telegramInlineRe.FindStringSubmatch(m); len(sub) > 1 {
			body = sub[1]
		}
		return protect("<code>" + html.EscapeString(body) + "</code>")
	})

	s = html.EscapeString(s)
	s = telegramLinkRe.ReplaceAllStringFunc(s, func(m string) string {
		sub := telegramLinkRe.FindStringSubmatch(m)
		if len(sub) < 3 {
			return m
		}
		url := html.UnescapeString(sub[2])
		return `<a href="` + html.EscapeString(url) + `">` + sub[1] + `</a>`
	})
	s = telegramBoldRe.ReplaceAllStringFunc(s, func(m string) string {
		sub := telegramBoldRe.FindStringSubmatch(m)
		inner := ""
		if len(sub) > 1 && sub[1] != "" {
			inner = sub[1]
		} else if len(sub) > 2 {
			inner = sub[2]
		}
		return "<b>" + inner + "</b>"
	})
	s = telegramItalicRe.ReplaceAllStringFunc(s, func(m string) string {
		sub := telegramItalicRe.FindStringSubmatch(m)
		if len(sub) < 2 || strings.TrimSpace(sub[1]) == "" {
			return m
		}
		return "<i>" + sub[1] + "</i>"
	})
	s = telegramStrikeRe.ReplaceAllStringFunc(s, func(m string) string {
		sub := telegramStrikeRe.FindStringSubmatch(m)
		if len(sub) < 2 {
			return m
		}
		return "<s>" + sub[1] + "</s>"
	})

	for i, fragment := range slots {
		s = strings.ReplaceAll(s, fmt.Sprintf("\x00K%d\x00", i), fragment)
	}
	return s
}

func telegramEntityError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "can't parse") ||
		strings.Contains(msg, "cant parse") ||
		strings.Contains(msg, "parse entities") ||
		strings.Contains(msg, "can't find end")
}
