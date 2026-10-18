package agent

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/fanice1998/kawa/internal/channel"
)

var batchItemMarkerRe = regexp.MustCompile(`(?:^|\s)(\d{1,2})[\.、\)）]\s*`)

// parseBatchFeatures detects requests like
// "分開製作以下功能 1. A 2. B 3. C 4. D" (inline or one item per line) and returns the header and items.
func parseBatchFeatures(text string) (string, []string, bool) {
	trimmed := strings.TrimSpace(text)
	if strings.HasPrefix(trimmed, "/") {
		return "", nil, false
	}
	hasSplitWord := false
	for _, kw := range []string{"分開", "分別", "依序", "逐一", "一個一個", "排程", "一項一項"} {
		if strings.Contains(trimmed, kw) {
			hasSplitWord = true
			break
		}
	}
	hasDevWord := false
	for _, kw := range []string{"製作", "實作", "開發", "做", "新增", "完成"} {
		if strings.Contains(trimmed, kw) {
			hasDevWord = true
			break
		}
	}
	if !hasSplitWord || !hasDevWord {
		return "", nil, false
	}

	locs := batchItemMarkerRe.FindAllStringSubmatchIndex(trimmed, -1)
	// Keep only markers that form a consecutive 1,2,3... sequence.
	var starts, bodies []int
	expected := 1
	for _, m := range locs {
		n, err := strconv.Atoi(trimmed[m[2]:m[3]])
		if err != nil || n != expected {
			continue
		}
		starts = append(starts, m[0])
		bodies = append(bodies, m[1])
		expected++
	}
	if len(starts) < 2 {
		return "", nil, false
	}

	header := strings.TrimSpace(trimmed[:starts[0]])
	var items []string
	for i := range starts {
		end := len(trimmed)
		if i+1 < len(starts) {
			end = starts[i+1]
		}
		item := strings.TrimSpace(trimmed[bodies[i]:end])
		if item != "" {
			items = append(items, item)
		}
	}
	if len(items) < 2 {
		return "", nil, false
	}
	return header, items, true
}

// goalChainContinues reports whether GOAL.md is in a chained mode and still has PENDING milestones.
func (a *Agent) goalChainContinues() bool {
	goal, err := a.loadGoalSpec()
	if err != nil || goal == nil {
		return false
	}
	if goal.AutoMode != "chain" && goal.AutoMode != "auto_approve" {
		return false
	}
	for _, m := range goal.Milestones {
		if m.Status == "PENDING" {
			return true
		}
	}
	return false
}

// startFeatureBatch turns a list of features into sequential GOAL.md milestones (chain mode):
// each milestone runs through the atomic TASK.md pipeline, waits for the user's /approve,
// hot-reloads, then the next milestone starts automatically. Features are never built concurrently.
func (a *Agent) startFeatureBatch(ctx context.Context, channelName, senderID, header string, items []string) string {
	vision := strings.TrimSpace(header)
	if vision == "" {
		vision = "依序完成使用者指定的多項功能"
	}
	goal := &GoalSpec{
		Vision:    vision,
		AutoMode:  "chain",
		UpdatedAt: time.Now().Format("2006-01-02 15:04:05"),
		Reflections: []string{
			fmt.Sprintf("[%s] 使用者要求分開依序製作 %d 項功能：%s", time.Now().Format("2006-01-02 15:04"), len(items), strings.Join(items, " / ")),
		},
	}
	for i, it := range items {
		goal.Milestones = append(goal.Milestones, GoalMilestone{
			ID:        fmt.Sprintf("m-%d", i+1),
			Title:     it,
			Rationale: fmt.Sprintf("使用者指定的第 %d/%d 項功能，獨立製作、獨立驗證。", i+1, len(items)),
			TaskPrompt: fmt.Sprintf("在 Kawa 中實作「%s」。這是使用者要求分開依序製作的功能之一（共 %d 項），本次只實作這一項，不要同時實作其他項目。若使用者能在對話中觸發，建立對應的 Tool。不要手改 RegisterCustomTools，註冊會依照建構子簽章自動寫入。一次寫不完時，工作流會把該步繼續拆小，直到註冊與呼叫方式都正確。",
				it, len(items)),
			Status: "PENDING",
		})
	}
	_ = a.writeGoalFiles(renderGoalMarkdown(goal), goal)
	a.recordAction(fmt.Sprintf("Started feature batch (%d items)", len(items)))

	// Kick off the first milestone through the serialized queue.
	go a.enqueueOrExecuteInbound(ctx, channel.InboundMessage{
		ChannelName: channelName,
		SenderID:    senderID,
		Text:        "/goal_step",
	})

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("📋 **已排入 %d 項功能，將「依序、一次只做一項」：**\n", len(items)))
	for i, it := range items {
		sb.WriteString(fmt.Sprintf("%d. %s\n", i+1, it))
	}
	sb.WriteString("\n流程：每項功能 ➔ 小步驟（做不完就再拆） ➔ 簽章註冊與呼叫檢查 ➔ 沙盒驗證 ➔ 等你 `/approve` ➔ 熱重啟 ➔ **自動接續下一項**。\n")
	sb.WriteString("• `/goal` 查看整體進度　• `/queue` 查看目前執行狀態　• `/cancel` 中斷當前項目\n")
	sb.WriteString("🚀 第 1 項已開始製作...")
	return sb.String()
}
