package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// SendRichMessageTool 用於發送富文本訊息的工具
type SendRichMessageTool struct {
	deps *CustomToolDeps
}

// NewSendRichMessageTool 建立 SendRichMessageTool 實例
func NewSendRichMessageTool(deps *CustomToolDeps) *SendRichMessageTool {
	return &SendRichMessageTool{deps: deps}
}

// Name 返回工具名稱
func (t *SendRichMessageTool) Name() string {
	return "send_rich_message"
}

// Description 返回工具描述
func (t *SendRichMessageTool) Description() string {
	return "發送富文本訊息給指定接收者。支援 Markdown 或 HTML 格式。"
}

// Parameters 返回工具參數定義
func (t *SendRichMessageTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"recipient": {
				"type": "string",
				"description": "接收者 ID。省略時使用目前對話的發送者"
			},
			"text": {
				"type": "string",
				"description": "要發送的富文本內容"
			},
			"channel": {
				"type": "string",
				"description": "通道名稱，例如 telegram。省略時使用目前對話的通道"
			}
		},
		"required": ["text"]
	}`)
}

// Execute 執行發送富文本訊息的操作
func (t *SendRichMessageTool) Execute(ctx context.Context, rawArgs string) (string, error) {
	var args struct {
		Recipient string `json:"recipient"`
		Text      string `json:"text"`
		Channel   string `json:"channel"`
	}
	if err := json.Unmarshal([]byte(rawArgs), &args); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	if t.deps == nil || t.deps.Router == nil {
		return "", fmt.Errorf("channel router is not initialized")
	}

	channelName, recipient := DeliveryTargetFromContext(ctx)
	if strings.TrimSpace(args.Channel) != "" {
		channelName = strings.TrimSpace(args.Channel)
	}
	if channelName == "" {
		channelName = "telegram"
	}
	if strings.TrimSpace(args.Recipient) != "" {
		recipient = strings.TrimSpace(args.Recipient)
	}
	if recipient == "" || args.Text == "" {
		return "", fmt.Errorf("recipient and text are required")
	}

	ch, ok := t.deps.Router.Channel(channelName)
	if !ok {
		return "", fmt.Errorf("channel %q not registered", channelName)
	}
	sender, ok := ch.(interface {
		SendRich(ctx context.Context, recipient string, text string) error
	})
	if !ok {
		return "", fmt.Errorf("channel %q does not support rich messages", channelName)
	}
	if err := sender.SendRich(ctx, recipient, args.Text); err != nil {
		return "", fmt.Errorf("failed to send rich message: %w", err)
	}

	return fmt.Sprintf("成功發送富訊息給 %s", recipient), nil
}
