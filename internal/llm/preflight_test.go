package llm

import (
	"strings"
	"testing"
)

func TestSanitizeAndValidateRequest(t *testing.T) {
	req := ChatRequest{
		Messages: []Message{
			{Role: RoleSystem, Content: "System prompt\x00with null"},
			{Role: RoleUser, Content: "First question"},
			{Role: RoleAssistant, Content: ""}, // Empty message without tool calls -> should be dropped
			{Role: RoleUser, Content: "Second question"},
			{Role: RoleAssistant, Content: "<think>\nLong internal reasoning...\n</think>\nFinal answer"},
		},
	}

	cleaned, report := SanitizeAndValidateRequest(req)
	if len(cleaned.Messages) != 3 {
		t.Fatalf("expected 3 cleaned messages after dropping empty and merging user messages, got %d", len(cleaned.Messages))
	}
	if strings.Contains(cleaned.Messages[0].Content, "\x00") {
		t.Fatalf("expected NUL byte to be removed")
	}
	if !strings.Contains(cleaned.Messages[1].Content, "First question") || !strings.Contains(cleaned.Messages[1].Content, "Second question") {
		t.Fatalf("expected consecutive user messages to be merged, got: %q", cleaned.Messages[1].Content)
	}
	if strings.Contains(cleaned.Messages[2].Content, "<think>") || cleaned.Messages[2].Content != "Final answer" {
		t.Fatalf("expected <think> block to be stripped, got: %q", cleaned.Messages[2].Content)
	}
	if len(report.FixesApplied) == 0 {
		t.Fatalf("expected report.FixesApplied to record fixes")
	}
}
