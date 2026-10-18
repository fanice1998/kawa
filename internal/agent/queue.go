package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fanice1998/kawa/internal/channel"
	"github.com/rs/zerolog/log"
)

// ActiveTaskInfo tracks the currently executing LLM turn or TASK.md development pipeline.
type ActiveTaskInfo struct {
	ChannelName string
	SenderID    string
	Text        string
	StartedAt   time.Time
	Cancel      context.CancelFunc
}

// isFastImmediateCommand returns true for lightweight read-only or control slash commands
// that should execute immediately without waiting behind a long LLM thinking turn or TASK.md build.
func isFastImmediateCommand(text string) bool {
	fields := strings.Fields(strings.TrimSpace(text))
	if len(fields) == 0 {
		return false
	}
	cmd := strings.ToLower(fields[0])
	if idx := strings.Index(cmd, "@"); idx != -1 {
		cmd = cmd[:idx]
	}
	switch cmd {
	case "/help", "/status", "/queue", "/cancel", "/stop", "/pending", "/jobs", "/memory", "/check", "/restart", "/sync_commands", "/provider", "/model", "/reset":
		return true
	default:
		return false
	}
}

// enqueueOrExecuteInbound routes fast control commands immediately and places LLM/development
// messages into a serialized FIFO queue so concurrent messages from multiple users or rapid
// follow-ups never interrupt or corrupt an ongoing thinking/coding turn.
func (a *Agent) enqueueOrExecuteInbound(ctx context.Context, msg channel.InboundMessage) {
	trimmed := strings.TrimSpace(msg.Text)
	if isFastImmediateCommand(trimmed) {
		go a.handleInbound(ctx, msg)
		return
	}

	a.queueMu.Lock()
	isBusy := a.activeTask != nil
	var activeSnapshot ActiveTaskInfo
	if isBusy {
		activeSnapshot = *a.activeTask
	}
	a.pendingQueue = append(a.pendingQueue, msg)
	queuePos := len(a.pendingQueue)
	a.queueMu.Unlock()

	if isBusy {
		elapsed := time.Since(activeSnapshot.StartedAt).Round(time.Second)
		notice := fmt.Sprintf(
			"📥 **[已排入訊息佇列 #%d]**\n"+
				"Kawa 目前正在處理前一則任務（`%s`，已進行 `%s`），不會被新訊息截斷思考。\n"+
				"您的訊息已安全排入佇列，待當前任務完成後將立即依序執行！\n"+
				"*(可輸入 `/queue` 查看佇列狀態，或 `/cancel` 中斷當前任務)*",
			queuePos,
			truncateLog(activeSnapshot.Text, 36),
			elapsed,
		)
		if err := a.router.Send(ctx, msg.ChannelName, msg.SenderID, notice); err != nil {
			log.Warn().Err(err).Msg("Failed to send queue position notice")
		}
	}

	// Signal the worker loop that a new item is available
	select {
	case a.queueNotify <- struct{}{}:
	default:
	}
}

// runQueueWorker processes queued messages strictly one-by-one in FIFO order.
func (a *Agent) runQueueWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-a.queueNotify:
			for {
				a.queueMu.Lock()
				if len(a.pendingQueue) == 0 {
					a.activeTask = nil
					a.queueMu.Unlock()
					break
				}
				nextMsg := a.pendingQueue[0]
				a.pendingQueue = a.pendingQueue[1:]

				taskCtx, taskCancel := context.WithCancel(ctx)
				a.activeTask = &ActiveTaskInfo{
					ChannelName: nextMsg.ChannelName,
					SenderID:    nextMsg.SenderID,
					Text:        nextMsg.Text,
					StartedAt:   time.Now(),
					Cancel:      taskCancel,
				}
				a.queueMu.Unlock()

				a.handleInbound(taskCtx, nextMsg)
				taskCancel()

				a.queueMu.Lock()
				a.activeTask = nil
				a.queueMu.Unlock()
			}
		}
	}
}

// formatQueueStatus returns a human-readable summary of the active task and waiting queue.
func (a *Agent) formatQueueStatus() string {
	a.queueMu.Lock()
	defer a.queueMu.Unlock()

	var sb strings.Builder
	sb.WriteString("📬 **[Kawa 訊息佇列與執行狀態]**\n")
	if a.activeTask == nil {
		sb.WriteString("• **當前狀態**: 🟢 閒置中（隨時可立即處理新訊息）\n")
	} else {
		elapsed := time.Since(a.activeTask.StartedAt).Round(time.Second)
		sb.WriteString(fmt.Sprintf("• **正在執行**: 🔄 `%s` (來自 `%s:%s`，已執行 `%s`)\n",
			truncateLog(a.activeTask.Text, 60),
			a.activeTask.ChannelName,
			a.activeTask.SenderID,
			elapsed,
		))
	}

	if len(a.pendingQueue) == 0 {
		sb.WriteString("• **等待佇列**: 目前無排隊訊息 (0 則)\n")
	} else {
		sb.WriteString(fmt.Sprintf("• **等待佇列 (%d 則)**:\n", len(a.pendingQueue)))
		for i, m := range a.pendingQueue {
			sb.WriteString(fmt.Sprintf("  %d. [%s:%s] `%s`\n", i+1, m.ChannelName, m.SenderID, truncateLog(m.Text, 50)))
		}
	}
	return strings.TrimSpace(sb.String())
}

// cancelActiveTask aborts the currently running LLM turn or TASK.md pipeline if one is active.
func (a *Agent) cancelActiveTask() string {
	a.queueMu.Lock()
	defer a.queueMu.Unlock()

	if a.activeTask != nil {
		cancelledText := truncateLog(a.activeTask.Text, 50)
		if a.activeTask.Cancel != nil {
			a.activeTask.Cancel()
		}
		remaining := len(a.pendingQueue)
		return fmt.Sprintf("🛑 已發送中斷訊號，取消當前任務：`%s`（佇列中尚有 %d 則待處理訊息）。", cancelledText, remaining)
	}

	if a.cfg.Sandbox.StateDir != "" {
		taskFile := filepath.Join(a.cfg.Sandbox.StateDir, "current_task.json")
		if _, err := os.Stat(taskFile); err == nil {
			_ = os.Remove(taskFile)
			rootTaskPath := filepath.Join(a.cfg.Agent.SourceDir, "TASK.md")
			_ = os.Remove(rootTaskPath)
			return "🛑 目前沒有正在執行中的即時對話任務。已為您清除停滯的 `TASK.md` 與 `.kawa/current_task.json` 任務暫存！"
		}
	}

	return "ℹ️ 目前沒有正在執行中的任務。"
}
