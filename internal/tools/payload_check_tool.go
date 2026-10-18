package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/fanice1998/kawa/internal/llm"
)

// PayloadCheckTool (`inspect_and_validate_payload`) allows Kawa or the user to inspect,
// validate, and sanitize text/JSON/SEARCH-REPLACE payloads before or during LLM interactions.
type PayloadCheckTool struct{}

func NewPayloadCheckTool() *PayloadCheckTool {
	return &PayloadCheckTool{}
}

func (t *PayloadCheckTool) Name() string {
	return "inspect_and_validate_payload"
}

func (t *PayloadCheckTool) Description() string {
	return "檢查並修復即將傳送給模型或從模型接收的內容格式（包含 UTF-8 編碼、控制字元、<think> 標籤清理、JSON 語法驗證、SEARCH/REPLACE 區塊完整性與 Token 長度預估）。"
}

func (t *PayloadCheckTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"content": {
				"type": "string",
				"description": "要檢查與修復的文字、JSON 或 SEARCH/REPLACE 內容"
			},
			"expected_format": {
				"type": "string",
				"enum": ["auto", "json", "search_replace", "chat_messages"],
				"description": "預期的內容格式 (預設為 auto)"
			}
		},
		"required": ["content"]
	}`)
}

func (t *PayloadCheckTool) Execute(ctx context.Context, rawArgs string) (string, error) {
	var args struct {
		Content        string `json:"content"`
		ExpectedFormat string `json:"expected_format"`
	}
	if err := json.Unmarshal([]byte(rawArgs), &args); err != nil {
		return "", fmt.Errorf("invalid arguments for inspect_and_validate_payload: %w", err)
	}

	format := strings.ToLower(strings.TrimSpace(args.ExpectedFormat))
	if format == "" {
		format = "auto"
	}

	raw := args.Content
	var issues []string
	var fixes []string

	// 1. Check UTF-8 validity
	if !utf8.ValidString(raw) {
		issues = append(issues, "發現無效的 UTF-8 字元序列")
		fixes = append(fixes, "已自動移除無效的 UTF-8 bytes")
	}

	// 2. Check and strip <think>...</think> blocks
	cleaned := llm.StripThinkingTags(raw)
	if cleaned != strings.TrimSpace(raw) && strings.Contains(strings.ToLower(raw), "<think") {
		issues = append(issues, "包含未清理的 `<think>...</think>` 思考區塊（可能干擾 JSON 或 Patch 解析）")
		fixes = append(fixes, "已自動剝除 `<think>` 思考標籤")
	}

	// 3. Run full message sanitization via SanitizeAndValidateRequest
	dummyReq := llm.ChatRequest{
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: cleaned},
		},
	}
	sanitizedReq, report := llm.SanitizeAndValidateRequest(dummyReq)
	if len(sanitizedReq.Messages) > 0 {
		cleaned = sanitizedReq.Messages[0].Content
	}
	if len(report.FixesApplied) > 0 {
		for _, fix := range report.FixesApplied {
			fixes = append(fixes, fmt.Sprintf("自動淨化規則觸發: `%s`", fix))
		}
	}

	// 4. Format-specific checks
	if format == "json" || (format == "auto" && (strings.HasPrefix(strings.TrimSpace(cleaned), "{") || strings.Contains(cleaned, "```json"))) {
		if json.Valid([]byte(strings.TrimSpace(cleaned))) {
			fixes = append(fixes, "JSON 語法驗證通過 (Valid JSON)")
		} else {
			issues = append(issues, "內容不是合法的純 JSON（可能包含多餘前後文或未閉合引號）")
		}
	}

	if format == "search_replace" || (format == "auto" && strings.Contains(cleaned, "<<<<<<< SEARCH")) {
		searchCount := strings.Count(cleaned, "<<<<<<< SEARCH")
		sepCount := strings.Count(cleaned, "=======")
		replaceCount := strings.Count(cleaned, ">>>>>>> REPLACE")
		if searchCount == 0 || searchCount != replaceCount || sepCount < searchCount {
			issues = append(issues, fmt.Sprintf("SEARCH/REPLACE 標記數量不對稱 (SEARCH=%d, =======%d, REPLACE=%d)", searchCount, sepCount, replaceCount))
		} else {
			fixes = append(fixes, fmt.Sprintf("SEARCH/REPLACE 區塊結構完整 (共 %d 個區塊)", searchCount))
		}
	}

	estTokens := report.TotalRunes / 2
	if estTokens == 0 && len(cleaned) > 0 {
		estTokens = 1
	}

	var sb strings.Builder
	sb.WriteString("🔍 **[Payload 格式與內容檢查報告]**\n")
	sb.WriteString(fmt.Sprintf("• **原始長度**: %d 字元 (%d bytes)\n", len([]rune(raw)), len(raw)))
	sb.WriteString(fmt.Sprintf("• **清理後長度**: %d 字元 (%d bytes, 預估 ~%d tokens)\n", len([]rune(cleaned)), len(cleaned), estTokens))

	if len(issues) == 0 {
		sb.WriteString("• **診斷結果**: ✅ 格式完全正常，無異常控制字元或結構損壞\n")
	} else {
		sb.WriteString("• **發現異常項目**:\n")
		for _, iss := range issues {
			sb.WriteString("  - ⚠️ " + iss + "\n")
		}
	}
	if len(fixes) > 0 {
		sb.WriteString("• **自動修復與驗證紀錄**:\n")
		for _, f := range fixes {
			sb.WriteString("  - 🛠️ " + f + "\n")
		}
	}

	preview := cleaned
	if len([]rune(preview)) > 600 {
		preview = string([]rune(preview)[:600]) + "...(已截斷)"
	}
	sb.WriteString(fmt.Sprintf("\n**清理後內容預覽**:\n```\n%s\n```", preview))
	return sb.String(), nil
}
