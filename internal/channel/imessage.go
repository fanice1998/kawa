package channel

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/fanice1998/kawa/internal/config"
	"github.com/rs/zerolog/log"
)

// IMessageChannel implements native macOS iMessage communication by polling ~/Library/Messages/chat.db
// in read-only mode via `/usr/bin/sqlite3 -json -readonly` and sending replies via parameterized AppleScript.
type IMessageChannel struct {
	chatDBPath     string
	pollInterval   time.Duration
	allowedHandles map[string]bool
	lastRowID      int64
}

func NewIMessageChannel(cfg config.IMessageChannelConfig) *IMessageChannel {
	allowed := make(map[string]bool, len(cfg.AllowedHandles))
	for _, h := range cfg.AllowedHandles {
		normalized := strings.ToLower(strings.TrimSpace(h))
		if normalized != "" {
			allowed[normalized] = true
		}
	}
	interval := time.Duration(cfg.PollIntervalMs) * time.Millisecond
	if interval < 500*time.Millisecond {
		interval = 2000 * time.Millisecond
	}
	return &IMessageChannel{
		chatDBPath:     cfg.ChatDBPath,
		pollInterval:   interval,
		allowedHandles: allowed,
	}
}

func (m *IMessageChannel) Name() string { return "imessage" }

type chatDBRow struct {
	RowID  int64  `json:"row_id"`
	Sender string `json:"sender"`
	Text   string `json:"text"`
}

func (m *IMessageChannel) Start(ctx context.Context, inbound chan<- InboundMessage) error {
	// Initialize lastRowID to current maximum ROWID so we only process newly arriving messages
	maxID, err := m.fetchMaxRowID(ctx)
	if err != nil {
		return fmt.Errorf("init iMessage chat.db (ensure terminal has Full Disk Access): %w", err)
	}
	m.lastRowID = maxID
	log.Info().Int64("start_row_id", m.lastRowID).Str("chat_db", m.chatDBPath).Msg("iMessage channel watching for new messages")

	ticker := time.NewTicker(m.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			rows, err := m.fetchNewMessages(ctx, m.lastRowID)
			if err != nil {
				log.Debug().Err(err).Msg("iMessage poll warning")
				continue
			}
			for _, r := range rows {
				if r.RowID > m.lastRowID {
					m.lastRowID = r.RowID
				}
				senderNorm := strings.ToLower(strings.TrimSpace(r.Sender))
				if len(m.allowedHandles) > 0 && !m.allowedHandles[senderNorm] {
					log.Warn().Str("sender", r.Sender).Msg("Ignored iMessage from non-whitelisted handle")
					continue
				}
				if strings.TrimSpace(r.Text) == "" {
					continue
				}
				inbound <- InboundMessage{
					ChannelName: m.Name(),
					SenderID:    strings.TrimSpace(r.Sender),
					Text:        strings.TrimSpace(r.Text),
				}
			}
		}
	}
}

// Send dispatches an iMessage using parameterized osascript (`on run argv`) so message content
// is never interpolated into AppleScript source code (preventing injection bugs).
func (m *IMessageChannel) Send(ctx context.Context, recipient string, text string) error {
	const appleScript = `on run argv
	set targetHandle to item 1 of argv
	set msgBody to item 2 of argv
	tell application "Messages"
		set targetService to 1st account whose service type = iMessage
		set targetBuddy to participant targetHandle of targetService
		send msgBody to targetBuddy
	end tell
end run`

	cmd := exec.CommandContext(ctx, "/usr/bin/osascript", "-e", appleScript, recipient, text)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("osascript send iMessage to %s failed: %w (%s)", recipient, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func (m *IMessageChannel) fetchMaxRowID(ctx context.Context) (int64, error) {
	query := "SELECT COALESCE(MAX(ROWID), 0) AS row_id FROM message;"
	out, err := m.runSQLiteReadOnly(ctx, query)
	if err != nil {
		return 0, err
	}
	if strings.TrimSpace(out) == "" {
		return 0, nil
	}
	var rows []chatDBRow
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		return 0, err
	}
	if len(rows) == 0 {
		return 0, nil
	}
	return rows[0].RowID, nil
}

func (m *IMessageChannel) fetchNewMessages(ctx context.Context, afterRowID int64) ([]chatDBRow, error) {
	query := fmt.Sprintf(`SELECT message.ROWID AS row_id, handle.id AS sender, message.text AS text
FROM message
JOIN handle ON message.handle_id = handle.ROWID
WHERE message.ROWID > %d AND message.is_from_me = 0 AND message.text IS NOT NULL
ORDER BY message.ROWID ASC
LIMIT 30;`, afterRowID)

	out, err := m.runSQLiteReadOnly(ctx, query)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(out) == "" {
		return nil, nil
	}
	var rows []chatDBRow
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		return nil, err
	}
	return rows, nil
}

func (m *IMessageChannel) runSQLiteReadOnly(ctx context.Context, sql string) (string, error) {
	cmd := exec.CommandContext(ctx, "/usr/bin/sqlite3", "-readonly", "-json", m.chatDBPath, sql)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("sqlite3: %w (%s)", err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}
