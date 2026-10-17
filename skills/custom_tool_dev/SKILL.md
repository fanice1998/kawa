---
name: custom_tool_dev
description: Kawa 自訂工具 (internal/tools/custom_tools.go) 開發指南：定義如何建立實作 Tool 介面的新工具檔案並註冊至 RegisterCustomTools。
---

# Kawa 自訂工具 (`internal/tools`) 開發指南

當使用者希望 Kawa 新增一個可自主呼叫的工具（例如：匯率換算、天氣查詢、系統資訊、API 查詢等）時，請拆分為以下 2 個微任務：

## 微任務 Step 1：建立新工具檔案 `internal/tools/<name>_tool.go`
每個工具只需實作 `internal/tools/tool.go` 中的 `Tool` 介面（**絕對不要修改 `internal/tools/tool.go`**）：

```go
package tools

import (
	"context"
	"encoding/json"
	"fmt"
)

type ExampleTool struct{}

func NewExampleTool() *ExampleTool {
	return &ExampleTool{}
}

func (t *ExampleTool) Name() string {
	return "example_tool"
}

func (t *ExampleTool) Description() string {
	return "工具用途說明（繁體中文）"
}

func (t *ExampleTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"input": {
				"type": "string",
				"description": "輸入參數說明"
			}
		},
		"required": ["input"]
	}`)
}

func (t *ExampleTool) Execute(ctx context.Context, rawArgs string) (string, error) {
	var args struct {
		Input string `json:"input"`
	}
	if err := json.Unmarshal([]byte(rawArgs), &args); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	return fmt.Sprintf("執行結果: %s", args.Input), nil
}
```

## 微任務 Step 2：在 `internal/tools/custom_tools.go` 註冊工具
使用 `SEARCH/REPLACE` 在 `RegisterCustomTools` 中加入一行 `reg.Register(...)`：

<<<<<<< SEARCH
func RegisterCustomTools(reg *Registry, deps CustomToolDeps) {
=======
func RegisterCustomTools(reg *Registry, deps CustomToolDeps) {
	reg.Register(NewExampleTool())
>>>>>>> REPLACE
