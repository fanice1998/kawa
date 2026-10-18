package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/fanice1998/kawa/internal/channel"
)

// DeleteMessageTool bridges the LLM to Kawa's self-evolved TelegramChannel.DeleteMessage via channel.Router.
type DeleteMessageTool struct {
	router *channel.Router
}

func NewDeleteMessageTool(router *channel.Router) *DeleteMessageTool {
	return &DeleteMessageTool{router: router}
}

func (t *DeleteMessageTool) Name() string {
	return "delete_message"
}

func (t *DeleteMessageTool) Description() string {
	return "刪除 Telegram 對話視窗中的指定訊息 (呼叫 Telegram deleteMessage API)。若使用者說「把我這則訊息刪除」或回覆某則訊息要求刪除，可不傳 message_id (或傳 0)，系統會自動帶入目標訊息的 message_id 與當前 chat_id。"
}

func (t *DeleteMessageTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"message_id": {
				"type": "integer",
				"description": "要刪除的 Telegram 訊息 ID (選填；若省略或為 0，會自動優先刪除被回覆的訊息或使用者當前的訊息)"
			},
			"chat_id": {
				"type": "string",
				"description": "目標聊天室 ID (選填；若省略則自動使用當前對話的 chat_id)"
			}
		}
	}`)
}

func (t *DeleteMessageTool) Execute(ctx context.Context, rawArgs string) (string, error) {
	if t.router == nil {
		return "", fmt.Errorf("channel router is not initialized")
	}

	var args struct {
		MessageID int    `json:"message_id"`
		ChatID    string `json:"chat_id"`
	}
	if strings.TrimSpace(rawArgs) != "" {
		_ = json.Unmarshal([]byte(rawArgs), &args)
	}

	chName, curChatID := DeliveryTargetFromContext(ctx)
	if chName == "" {
		chName = "telegram"
	}
	chatID := strings.TrimSpace(args.ChatID)
	if chatID == "" {
		chatID = curChatID
	}
	if chatID == "" {
		return "", fmt.Errorf("無法取得目標 chat_id")
	}

	curMsgID, replyMsgID := MessageMetaFromContext(ctx)
	targetMsgID := args.MessageID
	if targetMsgID <= 0 {
		if replyMsgID > 0 {
			targetMsgID = replyMsgID
		} else {
			targetMsgID = curMsgID
		}
	}
	if targetMsgID <= 0 {
		return "", fmt.Errorf("無法取得要刪除的 message_id（請直接回覆要刪除的訊息，或提供有效的 message_id）")
	}

	if err := t.router.DeleteMessage(ctx, chName, chatID, targetMsgID); err != nil {
		return "", fmt.Errorf("刪除訊息 (chat_id=%s, message_id=%d) 失敗: %w (提示：若在 Telegram 群組刪除他人或自己的訊息，請確認 Bot 擁有群組管理員的 Delete Messages 權限)", chatID, targetMsgID, err)
	}

	return fmt.Sprintf("✅ 已成功透過 `%s` 刪除訊息 (chat_id=`%s`, message_id=`%d`)！", chName, chatID, targetMsgID), nil
}
