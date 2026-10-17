---
name: telegram_channel_dev
description: Kawa Telegram 通道 (internal/channel/telegram.go) 擴充開發指南：包含 getUpdates 結構、Inline Query (answerInlineQuery)、指令同步與訊息發送的標準實作範本。
---

# Telegram Channel (`internal/channel/telegram.go`) 擴充開發指南

當需要為 Kawa 新增 Telegram 專屬功能（例如 **Inline Query 內聯查詢**、Callback Button、特殊訊息處理）時，請參考以下架構大綱與標準寫法：

## 1. `internal/channel/telegram.go` 現有關鍵結構
- `TelegramChannel` 結構體擁有：
  - `token string`
  - `allowedIDs map[int64]bool`
  - `pollSec int`
  - `client *http.Client`
- `tgUpdatesResp` 結構體（用於解析 `getUpdates` JSON）：
```go
type tgUpdatesResp struct {
	OK     bool `json:"ok"`
	Result []struct {
		UpdateID int64 `json:"update_id"`
		Message  *struct {
			Text string `json:"text"`
			From struct {
				ID       int64  `json:"id"`
				Username string `json:"username"`
			} `json:"from"`
			Chat struct {
				ID int64 `json:"id"`
			} `json:"chat"`
		} `json:"message"`
	} `json:"result"`
}
```

## 2. 實作 Telegram Inline Query 的標準微任務寫法

### 微任務 Step 1：擴充 `tgUpdatesResp` 並新增 `answerInlineQuery` 方法
在 `tgUpdatesResp` 加入 `InlineQuery` 欄位，並在 `TelegramChannel` 新增 `answerInlineQuery`：
<<<<<<< SEARCH
			Chat struct {
				ID int64 `json:"id"`
			} `json:"chat"`
		} `json:"message"`
	} `json:"result"`
}
=======
			Chat struct {
				ID int64 `json:"id"`
			} `json:"chat"`
		} `json:"message"`
		InlineQuery *struct {
			ID    string `json:"id"`
			Query string `json:"query"`
			From  struct {
				ID       int64  `json:"id"`
				Username string `json:"username"`
			} `json:"from"`
		} `json:"inline_query"`
	} `json:"result"`
}

func (t *TelegramChannel) answerInlineQuery(ctx context.Context, queryID, queryText string) {
	q := strings.TrimSpace(queryText)
	if q == "" {
		q = "請輸入想詢問 Kawa 的內容"
	}
	results := []map[string]any{
		{
			"type":  "article",
			"id":    "kawa-inline-1",
			"title": "詢問 Kawa: " + q,
			"description": "點選將此問題發送至對話視窗",
			"input_message_content": map[string]any{
				"message_text": q,
			},
		},
	}
	payload, _ := json.Marshal(map[string]any{
		"inline_query_id": queryID,
		"results":         results,
		"cache_time":      5,
	})
	url := fmt.Sprintf("https://api.telegram.org/bot%s/answerInlineQuery", t.token)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := t.client.Do(req)
	if err == nil {
		_ = resp.Body.Close()
	}
}
>>>>>>> REPLACE

### 微任務 Step 2：在 `Start()` 迴圈中處理 `u.InlineQuery`
<<<<<<< SEARCH
			if u.Message == nil || strings.TrimSpace(u.Message.Text) == "" {
				continue
			}
=======
			if u.InlineQuery != nil {
				if len(t.allowedIDs) == 0 || t.allowedIDs[u.InlineQuery.From.ID] {
					go t.answerInlineQuery(ctx, u.InlineQuery.ID, u.InlineQuery.Query)
				}
				continue
			}
			if u.Message == nil || strings.TrimSpace(u.Message.Text) == "" {
				continue
			}
>>>>>>> REPLACE
