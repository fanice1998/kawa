package channel

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/fanice1998/kawa/internal/config"
	"github.com/rs/zerolog/log"
)

// TelegramChannel implements long-polling Telegram Bot integration with user ID whitelisting.
type TelegramChannel struct {
	token      string
	allowedIDs map[int64]bool
	pollSec    int
	client     *http.Client
}

func (t *TelegramChannel) DeleteMessage(ctx context.Context, chatID string, messageID int) error {
	payload, _ := json.Marshal(map[string]any{
		"chat_id":    chatID,
		"message_id": messageID,
	})
	url := fmt.Sprintf("https://api.telegram.org/bot%s/deleteMessage", t.token)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := t.client.Do(req)
	if err != nil {
		return fmt.Errorf("telegram deleteMessage: %w", err)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 65536))
	_ = resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("telegram deleteMessage HTTP %d: %s", resp.StatusCode, string(body))
	}
	return nil
}

func NewTelegramChannel(cfg config.TelegramChannelConfig) *TelegramChannel {
	allowed := make(map[int64]bool, len(cfg.AllowedUserIDs))
	for _, id := range cfg.AllowedUserIDs {
		allowed[id] = true
	}
	poll := cfg.PollTimeoutSec
	if poll <= 0 {
		poll = 30
	}
	return &TelegramChannel{
		token:      strings.TrimSpace(cfg.BotToken),
		allowedIDs: allowed,
		pollSec:    poll,
		client:     &http.Client{Timeout: time.Duration(poll+15) * time.Second},
	}
}

func (t *TelegramChannel) Name() string { return "telegram" }

type tgUpdatesResp struct {
	OK     bool `json:"ok"`
	Result []struct {
		UpdateID int64 `json:"update_id"`
		Message  *struct {
			MessageID int    `json:"message_id"`
			Text      string `json:"text"`
			From      struct {
				ID       int64  `json:"id"`
				Username string `json:"username"`
			} `json:"from"`
			Chat struct {
				ID int64 `json:"id"`
			} `json:"chat"`
			ReplyToMessage *struct {
				MessageID int    `json:"message_id"`
				Text      string `json:"text"`
			} `json:"reply_to_message"`
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
			"type":        "article",
			"id":          "kawa-inline-1",
			"title":       "詢問 Kawa: " + q,
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

// BotCommand represents a command item in Telegram's native `/` menu.
type BotCommand struct {
	Command     string `json:"command"`
	Description string `json:"description"`
}

// DefaultBotCommands lists all commands automatically synced to Telegram via setMyCommands.
func DefaultBotCommands() []BotCommand {
	return []BotCommand{
		{Command: "goal", Description: "設定或查看長期抽象目標 (GOAL.md 演進藍圖)"},
		{Command: "goal_step", Description: "立即推進下一階段長期目標並執行自我審查"},
		{Command: "goal_auto", Description: "開關背景定時自主演進 (/goal_auto on|approve|off)"},
		{Command: "task", Description: "規劃單一新功能並產生 TASK.md 任務書"},
		{Command: "run_task", Description: "照著 TASK.md 自動分階段開發並測試"},
		{Command: "task_reset", Description: "清空當前停滯或出錯的 TASK.md 任務暫存 (/task_reset)"},
		{Command: "cancel", Description: "中斷目前執行中的任務或清空停滯暫存 (/cancel)"},
		{Command: "pending", Description: "查看待審核的自我進化 Diff 提案"},
		{Command: "approve", Description: "核准提案並自動重編譯與熱重啟 (/approve)"},
		{Command: "reject", Description: "拒絕並清除自我修改提案 (/reject <id>)"},
		{Command: "restart", Description: "立即執行自我熱重啟並回報上線通知"},
		{Command: "status", Description: "查看模型、沙盒、記憶庫與排程狀態"},
		{Command: "memory", Description: "查看核心記憶 (MEMORY.md) 與 RAG 記憶"},
		{Command: "jobs", Description: "查看執行中的定時排程任務"},
		{Command: "provider", Description: "切換 AI 模型 (/provider <name>)"},
		{Command: "sync_commands", Description: "手動將指令清單同步更新至 Telegram 選單"},
		{Command: "reset", Description: "清除當前短期對話記憶"},
		{Command: "help", Description: "顯示所有指令與開發流程說明"},
	}
}

func (t *TelegramChannel) Start(ctx context.Context, inbound chan<- InboundMessage) error {
	if t.token == "" {
		return fmt.Errorf("telegram bot_token is empty")
	}
	if err := t.SyncCommands(ctx, DefaultBotCommands()); err != nil {
		log.Warn().Err(err).Msg("Failed to auto-sync Telegram bot commands on startup")
	} else {
		log.Info().Int("commands", len(DefaultBotCommands())).Msg("Successfully synced Telegram bot commands menu (setMyCommands)")
	}
	var offset int64

	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		url := fmt.Sprintf("https://api.telegram.org/bot%s/getUpdates?offset=%d&timeout=%d", t.token, offset, t.pollSec)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return err
		}

		resp, err := t.client.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			log.Warn().Err(err).Msg("Telegram getUpdates failed, retrying in 3s")
			time.Sleep(3 * time.Second)
			continue
		}

		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024))
		_ = resp.Body.Close()

		var updates tgUpdatesResp
		if err := json.Unmarshal(body, &updates); err != nil || !updates.OK {
			snippet := string(body)
			if len(snippet) > 300 {
				snippet = snippet[:300]
			}
			log.Warn().Err(err).Str("body", snippet).Msg("Telegram getUpdates returned unparsable or non-OK response, retrying in 3s")
			time.Sleep(3 * time.Second)
			continue
		}

		for _, u := range updates.Result {
			if u.UpdateID >= offset {
				offset = u.UpdateID + 1
			}
			if u.InlineQuery != nil {
				if len(t.allowedIDs) == 0 || t.allowedIDs[u.InlineQuery.From.ID] {
					go t.answerInlineQuery(ctx, u.InlineQuery.ID, u.InlineQuery.Query)
				}
				continue
			}
			if u.Message == nil || strings.TrimSpace(u.Message.Text) == "" {
				continue
			}
			userID := u.Message.From.ID
			chatID := u.Message.Chat.ID

			if len(t.allowedIDs) > 0 && !t.allowedIDs[userID] {
				log.Warn().
					Int64("user_id", userID).
					Str("username", u.Message.From.Username).
					Msg("Ignored Telegram message from non-whitelisted user")
				continue
			}

			replyMsgID := 0
			if u.Message.ReplyToMessage != nil {
				replyMsgID = u.Message.ReplyToMessage.MessageID
			}

			inbound <- InboundMessage{
				ChannelName:      t.Name(),
				SenderID:         strconv.FormatInt(chatID, 10),
				Text:             strings.TrimSpace(u.Message.Text),
				MessageID:        u.Message.MessageID,
				ReplyToMessageID: replyMsgID,
			}
		}
	}
}

// StartTyping sends `typing` action immediately and repeats every 4 seconds until the returned stop func is called.
// (Telegram's typing status expires after 5 seconds unless refreshed.)
func (t *TelegramChannel) StartTyping(ctx context.Context, recipient string) func() {
	typingCtx, cancel := context.WithCancel(ctx)
	go func() {
		t.sendTypingAction(typingCtx, recipient)
		ticker := time.NewTicker(4 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-typingCtx.Done():
				return
			case <-ticker.C:
				t.sendTypingAction(typingCtx, recipient)
			}
		}
	}()
	return cancel
}

func (t *TelegramChannel) sendTypingAction(ctx context.Context, chatID string) {
	payload, _ := json.Marshal(map[string]any{
		"chat_id": chatID,
		"action":  "typing",
	})
	url := fmt.Sprintf("https://api.telegram.org/bot%s/sendChatAction", t.token)
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

// SyncCommands calls Telegram Bot API `setMyCommands` so the `/` command menu is automatically updated in Telegram clients.
func (t *TelegramChannel) SyncCommands(ctx context.Context, commands []BotCommand) error {
	payload, err := json.Marshal(map[string]any{
		"commands": commands,
	})
	if err != nil {
		return err
	}
	url := fmt.Sprintf("https://api.telegram.org/bot%s/setMyCommands", t.token)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := t.client.Do(req)
	if err != nil {
		return fmt.Errorf("telegram setMyCommands: %w", err)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 65536))
	_ = resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("telegram setMyCommands HTTP %d: %s", resp.StatusCode, string(body))
	}
	return nil
}

func (t *TelegramChannel) Send(ctx context.Context, recipient string, text string) error {
	for _, chunk := range splitMessageChunks(text, 3900) {
		payload, _ := json.Marshal(map[string]any{
			"chat_id": recipient,
			"text":    chunk,
		})
		url := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", t.token)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := t.client.Do(req)
		if err != nil {
			return fmt.Errorf("telegram sendMessage: %w", err)
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 65536))
		_ = resp.Body.Close()

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return fmt.Errorf("telegram sendMessage HTTP %d: %s", resp.StatusCode, string(body))
		}
	}
	return nil
}

func (t *TelegramChannel) SendStream(ctx context.Context, recipient string, chunks <-chan string) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case chunk, ok := <-chunks:
			if !ok {
				return nil
			}
			if strings.TrimSpace(chunk) == "" {
				continue
			}
			if err := t.Send(ctx, recipient, chunk); err != nil {
				return err
			}
		}
	}
}

func splitMessageChunks(s string, maxLen int) []string {
	if len(s) <= maxLen {
		return []string{s}
	}
	var chunks []string
	for len(s) > maxLen {
		chunks = append(chunks, s[:maxLen])
		s = s[maxLen:]
	}
	if len(s) > 0 {
		chunks = append(chunks, s)
	}
	return chunks
}
