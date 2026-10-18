package agent

import (
	"strings"
	"testing"

	"github.com/fanice1998/kawa/internal/llm"
)

func TestIsTaskExecutionTrigger(t *testing.T) {
	hist := []llm.Message{
		{Role: llm.RoleUser, Content: "可以幫我實作 Telegram inline 功能嗎？"},
		{Role: llm.RoleAssistant, Content: "好的，我們可以修改 internal/channel/telegram.go"},
	}

	if !isTaskExecutionTrigger("就這樣做！", hist) {
		t.Fatalf("expected '就這樣做！' to trigger task pipeline")
	}
	if !isTaskExecutionTrigger("幫我做成 TASK.md 並開始實作", nil) {
		t.Fatalf("expected explicit TASK.md trigger to return true")
	}
	if !isTaskExecutionTrigger("繼續實作 刪除 Telegram 訊息", nil) {
		t.Fatalf("expected '繼續實作 刪除 Telegram 訊息' to trigger task pipeline")
	}
	if !isTaskExecutionTrigger("好，開始做", hist) {
		t.Fatalf("expected short confirmation with feature history to return true")
	}
	if isTaskExecutionTrigger("今天天氣如何？", hist) {
		t.Fatalf("did not expect unrelated question to trigger task pipeline")
	}
}

func TestFastImmediateCommandAndQueueStatus(t *testing.T) {
	if !isFastImmediateCommand("/queue@fanice_kawa_bot") {
		t.Fatalf("expected /queue@fanice_kawa_bot to be recognized as fast immediate command")
	}
	if !isFastImmediateCommand("/cancel") {
		t.Fatalf("expected /cancel to be recognized as fast immediate command")
	}
	if isFastImmediateCommand("/run_task@fanice_kawa_bot") {
		t.Fatalf("did not expect /run_task to bypass the heavy task queue")
	}
}

func TestExtractFirstJSONObject(t *testing.T) {
	raw := "好的，以下是規劃好的 JSON：\n```json\n{\n  \"title\": \"Inline Query\",\n  \"target_files\": [\"internal/channel/telegram.go\"]\n}\n```\n請參考。"
	got := extractFirstJSONObject(raw)
	if !strings.Contains(got, `"title": "Inline Query"`) {
		t.Fatalf("unexpected extracted JSON: %s", got)
	}
}

func TestRenderTaskMarkdownCheckboxes(t *testing.T) {
	spec := &TaskSpec{
		Title:        "新增匯率查詢工具",
		Summary:      "實作匯率轉換 Tool",
		Requirements: []string{"支援 USD/TWD"},
		TargetFiles:  []string{"internal/tools/custom_tools.go"},
		Steps: []MicroStep{
			{ID: "step-1", Title: "建立工具檔案", TargetFile: "internal/tools/fx_tool.go", Done: true},
			{ID: "step-2", Title: "註冊工具", TargetFile: "internal/tools/custom_tools.go", Done: false},
		},
		Stage1Done: true,
		Stage2Done: false,
		Stage3Done: false,
		UpdatedAt:  "2026-10-18 05:00:00",
	}
	md := renderTaskMarkdown(spec)
	if !strings.Contains(md, "- [x] **Stage 1**") || !strings.Contains(md, "- [x] `step-1`") || !strings.Contains(md, "- [ ] `step-2`") {
		t.Fatalf("unexpected stage checkboxes in TASK.md:\n%s", md)
	}
}

func TestIsIncompleteToolAttemptClosedCodeBlock(t *testing.T) {
	closedExample := "以下是一段範例程式碼：\n```go\npackage main\nfunc main() {}\n```"
	if isIncompleteToolAttempt(closedExample) {
		t.Fatalf("closed code block should not be treated as an incomplete tool attempt")
	}

	danglingFence := "好的，我現在馬上查詢：\n```"
	if !isIncompleteToolAttempt(danglingFence) {
		t.Fatalf("unclosed trailing fence should be detected as incomplete tool attempt")
	}
}

func TestParseTaskPatchesSearchReplaceWithMarkdownFences(t *testing.T) {
	existing := map[string]string{
		"internal/channel/telegram.go": "package channel\n\nfunc Hello() string {\n\treturn \"old\"\n}\n",
	}
	// Simulate 14B model accidentally wrapping inner content with ```go ... ``` inside <<<<<<< SEARCH
	llmOutput := "### FILE: internal/channel/telegram.go\n<<<<<<< SEARCH\n```go\nfunc Hello() string {\n\treturn \"old\"\n}\n```\n=======\n```go\nfunc Hello() string {\n\treturn \"new\"\n}\n```\n>>>>>>> REPLACE\n"

	patches, err := parseTaskPatchesFromResponse(llmOutput, existing)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(patches) != 1 || !strings.Contains(patches[0].Content, `return "new"`) || strings.Contains(patches[0].Content, "```") {
		t.Fatalf("unexpected patch output: %+v", patches)
	}
}

func TestASTOutlineAndSymbolSlicing(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("package demo\n\nimport \"fmt\"\n\n")
	sb.WriteString("type Alpha struct {\n\tID int\n}\n\n")
	for i := 0; i < 120; i++ {
		sb.WriteString("// padding line\n")
	}
	sb.WriteString("func TargetFunc() {\n\tfmt.Println(\"hello\")\n}\n")
	src := sb.String()

	outline := buildGoFileOutline("demo.go", src)
	if !strings.Contains(outline, `symbol: "Alpha"`) || !strings.Contains(outline, `symbol: "TargetFunc"`) {
		t.Fatalf("unexpected AST outline:\n%s", outline)
	}

	sliced := sliceGoFileForSymbols(src, []string{"TargetFunc"}, "modify TargetFunc")
	if !strings.Contains(sliced, "func TargetFunc()") || strings.Contains(sliced, "type Alpha struct") {
		t.Fatalf("expected sliced code to contain TargetFunc and omit unrelated Alpha struct:\n%s", sliced)
	}
}
