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

func (t *TelegramChannel) Start(ctx context.Context, inbound chan<- InboundMessage) error {
	if t.token == "" {
		return fmt.Errorf("telegram bot_token is empty")
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
			time.Sleep(3 * time.Second)
			continue
		}

		for _, u := range updates.Result {
			if u.UpdateID >= offset {
				offset = u.UpdateID + 1
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

			// Immediately send "typing..." indicator so user sees Kawa is working
			go t.sendTypingAction(ctx, strconv.FormatInt(chatID, 10))

			inbound <- InboundMessage{
				ChannelName: t.Name(),
				SenderID:    strconv.FormatInt(chatID, 10),
				Text:        strings.TrimSpace(u.Message.Text),
			}
		}
	}
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
