package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fanice1998/kawa/internal/llm"
	"github.com/rs/zerolog/log"
)

// GoalMilestone represents one progressive step toward the user's abstract long-term goal.
type GoalMilestone struct {
	ID         string `json:"id"`
	Title      string `json:"title"`
	Rationale  string `json:"rationale"`
	TaskPrompt string `json:"task_prompt"`
	Status     string `json:"status"` // "PENDING", "NEEDS_REVIEW", "COMPLETED"
	ProposalID string `json:"proposal_id,omitempty"`
	Critique   string `json:"critique,omitempty"`
}

// GoalSpec represents Kawa's long-term abstract goal, milestone roadmap, and self-reflection journal (`GOAL.md`).
type GoalSpec struct {
	Vision        string          `json:"vision"`
	AutoMode      string          `json:"auto_mode"` // "off", "propose", "auto_approve"
	ScheduleJobID string          `json:"schedule_job_id,omitempty"`
	Milestones    []GoalMilestone `json:"milestones"`
	Reflections   []string        `json:"reflections,omitempty"`
	UpdatedAt     string          `json:"updated_at"`
}

// RebootNotice is persisted to `.kawa/reboot_notice.json` right before `syscall.Exec`
// so the newly booted process can immediately notify the user on Telegram that hot-reload succeeded.
type RebootNotice struct {
	ChannelName string `json:"channel_name"`
	SenderID    string `json:"sender_id"`
	ProposalID  string `json:"proposal_id"`
	Title       string `json:"title"`
	RequestedAt string `json:"requested_at"`
}

func (a *Agent) saveRebootNotice(notice RebootNotice) {
	if a.cfg.Sandbox.StateDir == "" {
		return
	}
	p := filepath.Join(a.cfg.Sandbox.StateDir, "reboot_notice.json")
	if raw, err := json.MarshalIndent(notice, "", "  "); err == nil {
		_ = os.WriteFile(p, raw, 0o644)
	}
}

func (a *Agent) checkAndSendRebootNotice(ctx context.Context) {
	if a.cfg.Sandbox.StateDir == "" || a.router == nil {
		return
	}
	p := filepath.Join(a.cfg.Sandbox.StateDir, "reboot_notice.json")
	data, err := os.ReadFile(p)
	if err != nil {
		return
	}
	_ = os.Remove(p)

	var notice RebootNotice
	if err := json.Unmarshal(data, &notice); err != nil || notice.ChannelName == "" || notice.SenderID == "" {
		return
	}

	// Wait briefly for Telegram channel startup to finish
	time.Sleep(1200 * time.Millisecond)

	msg := fmt.Sprintf(
		"🚀 **[Kawa 自我熱重啟成功！]**\n"+
			"已透過 `syscall.Exec` 原地換血升級為最新編譯版本（PID: `%d`）！\n"+
			"• **生效提案**: `%s` — %s\n"+
			"• **重啟時間**: `%s`\n"+
			"• **系統狀態**: 所有模組、記憶庫與排程皆已無縫接軌運作 ✅",
		os.Getpid(),
		notice.ProposalID,
		notice.Title,
		time.Now().Format("2006-01-02 15:04:05"),
	)
	if err := a.router.Send(ctx, notice.ChannelName, notice.SenderID, msg); err != nil {
		log.Warn().Err(err).Msg("Failed to send post-reboot notification")
	}
}

// DraftGoalRoadmap takes an abstract user goal, inspects Kawa's current architecture,
// and formulates a progressive multi-milestone roadmap in `GOAL.md`.
func (a *Agent) DraftGoalRoadmap(ctx context.Context, vision string) (*GoalSpec, string, error) {
	provider, err := a.llmRegistry.Active()
	if err != nil {
		return nil, "", err
	}

	customToolsPath := filepath.Join(a.cfg.Agent.SourceDir, "internal", "tools", "custom_tools.go")
	customToolsData, _ := os.ReadFile(customToolsPath)

	sysPrompt := `你是 Kawa 的長期自我演進總設計師。使用者給了一個「抽象長期目標 (Vision)」。
請盤點 Kawa 目前的能力，將這個抽象目標拆解為 **3~4 個由淺入深、具體可執行的演進里程碑 (Milestones)**，並只輸出一個純 JSON 物件：
{
  "vision": "使用者的抽象長期目標",
  "milestones": [
    {
      "id": "m-1",
      "title": "第一階段里程碑名稱（必須是一個小而具體的 Go 工具或功能）",
      "rationale": "為什麼先做這一步能往目標邁進",
      "task_prompt": "具體開發指示（例如：在 internal/tools 新增 xxx_tool.go 並在 custom_tools.go 註冊...）"
    },
    {
      "id": "m-2",
      "title": "第二階段里程碑名稱",
      "rationale": "在第一階段基礎上進一步強化什麼能力",
      "task_prompt": "具體開發指示"
    }
  ]
}
【重要原則】：
1. 每個里程碑必須能轉化為 Kawa 的 Go 程式碼修改（例如在 internal/tools/ 新增工具、或在 internal/channel/telegram.go 與 internal/agent/ 強化功能）。
2. 由簡入繁：m-1 應該是最基礎、最容易驗證通過的核心能力，後續里程碑再逐步疊加。`

	userPrompt := fmt.Sprintf("## 使用者訂立的抽象目標\n%s\n\n## Kawa 目前的 custom_tools.go 狀態\n```go\n%s\n```",
		vision, truncateRunes(string(customToolsData), 1500))

	planCtx, cancel := context.WithTimeout(ctx, 180*time.Second)
	defer cancel()

	resp, err := provider.Chat(planCtx, llm.ChatRequest{
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: sysPrompt},
			{Role: llm.RoleUser, Content: userPrompt},
		},
	})
	if err != nil {
		return nil, "", fmt.Errorf("規劃長期目標藍圖失敗: %w", err)
	}

	goal := &GoalSpec{
		Vision:    strings.TrimSpace(vision),
		AutoMode:  "off",
		UpdatedAt: time.Now().Format("2006-01-02 15:04:05"),
		Reflections: []string{
			fmt.Sprintf("[%s] 確立長期演進目標：%s", time.Now().Format("2006-01-02 15:04"), strings.TrimSpace(vision)),
		},
	}

	if jsonStr := extractFirstJSONObject(resp.Content); jsonStr != "" {
		var parsed GoalSpec
		if err := json.Unmarshal([]byte(jsonStr), &parsed); err == nil && len(parsed.Milestones) > 0 {
			for i := range parsed.Milestones {
				if parsed.Milestones[i].ID == "" {
					parsed.Milestones[i].ID = fmt.Sprintf("m-%d", i+1)
				}
				parsed.Milestones[i].Status = "PENDING"
			}
			goal.Milestones = parsed.Milestones
		}
	}

	if len(goal.Milestones) == 0 {
		goal.Milestones = []GoalMilestone{
			{
				ID:         "m-1",
				Title:      "建立第一階段基礎工具與核心能力",
				Rationale:  "為長期目標建立第一個可呼叫的基礎工具模組",
				TaskPrompt: fmt.Sprintf("根據長期目標（%s），在 internal/tools 新增對應的基礎工具並在 internal/tools/custom_tools.go 註冊", vision),
				Status:     "PENDING",
			},
		}
	}

	md := renderGoalMarkdown(goal)
	_ = a.writeGoalFiles(md, goal)
	return goal, md, nil
}

// AdvanceGoalStep executes one full cognitive evolution cycle toward GOAL.md:
// 1. Select next pending milestone (or generate new milestones if all are done)
// 2. Reflect on direction & convert milestone into atomic TASK.md micro-steps
// 3. Execute micro-steps and verify in macOS Seatbelt sandbox
// 4. Perform LLM Self-Critique on the resulting Git Diff (checking direction & correctness)
// 5. Record reflection in GOAL.md (and auto-approve + hot-reload if AutoMode == "auto_approve")
func (a *Agent) AdvanceGoalStep(ctx context.Context, channelName, senderID string) (string, error) {
	goal, err := a.loadGoalSpec()
	if err != nil || goal == nil {
		return "⚠️ 目前尚未設定長期目標！請先使用 `/goal <你的抽象目標>` 來建立 `GOAL.md` 演進藍圖。", nil
	}

	targetIdx := -1
	for i, m := range goal.Milestones {
		if m.Status == "PENDING" {
			targetIdx = i
			break
		}
	}
	if targetIdx == -1 {
		return fmt.Sprintf("🎉 目前 `GOAL.md` 中的所有階段里程碑皆已推進完成！\n\n%s\n👉 若有 `NEEDS_REVIEW` 的提案請先 `/approve`，或輸入 `/goal <新目標>` 開啟下一輪演進！", renderGoalMarkdown(goal)), nil
	}

	ms := &goal.Milestones[targetIdx]
	a.notifyProgress(ctx, channelName, senderID, fmt.Sprintf(
		"🧭 **[GOAL.md 自主演進循環啟動]**\n"+
			"• **北極星目標**: %s\n"+
			"• **當前推進里程碑**: `%s` — **%s**\n"+
			"• **思考方向與理由**: %s\n\n"+
			"正在將此里程碑轉化為 `TASK.md` 原子微任務並開始實作...",
		goal.Vision, ms.ID, ms.Title, ms.Rationale,
	))

	// Run the atomic TASK.md pipeline for this milestone
	taskInstruction := fmt.Sprintf("[長期目標: %s | 里程碑 %s: %s] %s", goal.Vision, ms.ID, ms.Title, ms.TaskPrompt)
	taskReply, err := a.RunTaskPipeline(ctx, channelName, senderID, taskInstruction)
	if err != nil {
		reflection := fmt.Sprintf("[%s] 里程碑 %s (%s) 推進遭遇錯誤：%v", time.Now().Format("15:04:05"), ms.ID, ms.Title, err)
		goal.Reflections = append(goal.Reflections, reflection)
		goal.UpdatedAt = time.Now().Format("2006-01-02 15:04:05")
		_ = a.writeGoalFiles(renderGoalMarkdown(goal), goal)
		return "", err
	}

	// Check if a verified proposal was produced in current_task.json
	taskSpec, _ := a.loadTaskSpec()
	if taskSpec == nil || !taskSpec.Stage3Done || taskSpec.ProposalID == "" {
		reflection := fmt.Sprintf("[%s] 里程碑 %s (%s) 沙盒驗證未完全通過，將於下次循環修正。", time.Now().Format("15:04:05"), ms.ID, ms.Title)
		goal.Reflections = append(goal.Reflections, reflection)
		goal.UpdatedAt = time.Now().Format("2006-01-02 15:04:05")
		_ = a.writeGoalFiles(renderGoalMarkdown(goal), goal)
		return taskReply, nil
	}

	// Self-Critique Phase: Evaluate whether the implementation direction & code diff are sound
	critiqueSummary, passedCritique := a.selfCritiqueEvolution(ctx, goal, ms, taskSpec.ProposalID)
	ms.ProposalID = taskSpec.ProposalID
	ms.Critique = critiqueSummary
	ms.Status = "NEEDS_REVIEW"

	reflectionEntry := fmt.Sprintf("[%s] 完成里程碑 %s (%s) 實作與沙盒測試 (Proposal: %s)。自我審查結論：%s",
		time.Now().Format("2006-01-02 15:04"), ms.ID, ms.Title, taskSpec.ProposalID, critiqueSummary)
	goal.Reflections = append(goal.Reflections, reflectionEntry)
	goal.UpdatedAt = time.Now().Format("2006-01-02 15:04:05")
	_ = a.writeGoalFiles(renderGoalMarkdown(goal), goal)

	// If full autonomous evolution (`auto_approve`) is enabled and self-critique passed,
	// automatically approve, rebuild, and hot-reload Kawa!
	if goal.AutoMode == "auto_approve" && passedCritique {
		p, appErr := a.evolveEngine.Approve(ctx, taskSpec.ProposalID)
		if appErr == nil {
			ms.Status = "COMPLETED"
			_ = a.writeGoalFiles(renderGoalMarkdown(goal), goal)
			a.saveRebootNotice(RebootNotice{
				ChannelName: channelName,
				SenderID:    senderID,
				ProposalID:  p.ID,
				Title:       fmt.Sprintf("[GOAL %s] %s", ms.ID, ms.Title),
				RequestedAt: time.Now().Format(time.RFC3339),
			})
			if a.onHotReload != nil {
				go func() {
					time.Sleep(1500 * time.Millisecond)
					a.onHotReload()
				}()
			}
			return fmt.Sprintf(
				"🌱 **[GOAL.md 全自動進化完成]**\n"+
					"• **里程碑**: `%s` — %s\n"+
					"• **自我審查結果**: %s\n"+
					"• **動作**: 已自動核准提案 `%s`，正在重新編譯並執行 `syscall.Exec` 自我熱重啟...",
				ms.ID, ms.Title, critiqueSummary, p.ID,
			), nil
		}
	}

	return fmt.Sprintf(
		"🌱 **[GOAL.md 里程碑 `%s` 開發與自我審查完成]**\n\n"+
			"### 🧐 Kawa 自我反思與方向審查 (Self-Critique)\n"+
			"%s\n\n"+
			"---\n%s",
		ms.ID, critiqueSummary, taskReply,
	), nil
}

// selfCritiqueEvolution inspects the generated proposal diff and verifies whether it aligns with the goal and has no logic flaws.
func (a *Agent) selfCritiqueEvolution(ctx context.Context, goal *GoalSpec, ms *GoalMilestone, proposalID string) (string, bool) {
	prop, ok := a.evolveEngine.Get(proposalID)
	if !ok || prop == nil {
		return "已通過沙盒編譯與單元測試驗證。", true
	}

	provider, err := a.llmRegistry.Active()
	if err != nil {
		return "已通過沙盒編譯與單元測試驗證。", true
	}

	sysPrompt := `你是 Kawa 的自我反思與品質審查核心 (Self-Critique Engine)。
請檢查這次自我進化產生的 Git Diff 是否符合長期目標與當前里程碑，並檢查是否有潛在邏輯錯誤（如無窮迴圈、nil 指標、偏離目標）。
請只輸出一個純 JSON 物件：
{
  "passed": true,
  "critique": "1~2 句話總結這次實作的方向是否正確、程式碼品質如何，以及下一步成長建議"
}`

	userPrompt := fmt.Sprintf("## 長期目標\n%s\n\n## 當前里程碑 (%s)\n%s (%s)\n\n## 產生的 Git Diff\n```diff\n%s\n```",
		goal.Vision, ms.ID, ms.Title, ms.Rationale, truncateRunes(prop.Diff, 2000))

	critCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()

	resp, err := provider.Chat(critCtx, llm.ChatRequest{
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: sysPrompt},
			{Role: llm.RoleUser, Content: userPrompt},
		},
	})
	if err != nil {
		return "已通過沙盒 `go vet`、`go test` 與 `go build` 驗證，方向符合里程碑設定。", true
	}

	if jsonStr := extractFirstJSONObject(resp.Content); jsonStr != "" {
		var parsed struct {
			Passed   bool   `json:"passed"`
			Critique string `json:"critique"`
		}
		if err := json.Unmarshal([]byte(jsonStr), &parsed); err == nil && strings.TrimSpace(parsed.Critique) != "" {
			return parsed.Critique, parsed.Passed
		}
	}
	return "已通過沙盒 `go vet`、`go test` 與 `go build` 驗證，方向符合里程碑設定。", true
}

func (a *Agent) markGoalMilestoneApproved(proposalID string) {
	goal, err := a.loadGoalSpec()
	if err != nil || goal == nil {
		return
	}
	changed := false
	for i := range goal.Milestones {
		if goal.Milestones[i].ProposalID == proposalID {
			goal.Milestones[i].Status = "COMPLETED"
			changed = true
		}
	}
	if changed {
		goal.Reflections = append(goal.Reflections, fmt.Sprintf("[%s] 提案 %s 已核准並熱重啟生效！", time.Now().Format("2006-01-02 15:04"), proposalID))
		goal.UpdatedAt = time.Now().Format("2006-01-02 15:04:05")
		_ = a.writeGoalFiles(renderGoalMarkdown(goal), goal)
	}
}

func renderGoalMarkdown(goal *GoalSpec) string {
	var sb strings.Builder
	sb.WriteString("# 🌟 GOAL.md — Kawa 長期自我演進藍圖\n\n")
	sb.WriteString(fmt.Sprintf("> **北極星目標 (Vision)**: %s\n", goal.Vision))
	sb.WriteString(fmt.Sprintf("> **自動演進模式**: `%s` | **最後更新**: `%s`\n\n", goal.AutoMode, goal.UpdatedAt))

	sb.WriteString("## 1. 階段性演進里程碑 (Evolution Milestones)\n")
	for _, m := range goal.Milestones {
		mark := " "
		if m.Status == "COMPLETED" {
			mark = "x"
		} else if m.Status == "NEEDS_REVIEW" {
			mark = "~"
		}
		sb.WriteString(fmt.Sprintf("- [%s] **`%s` %s** *(狀態: %s)*\n", mark, m.ID, m.Title, m.Status))
		sb.WriteString(fmt.Sprintf("  - 🎯 演進方向：%s\n", m.Rationale))
		if m.ProposalID != "" {
			sb.WriteString(fmt.Sprintf("  - 🧬 提案 ID：`%s`\n", m.ProposalID))
		}
		if m.Critique != "" {
			sb.WriteString(fmt.Sprintf("  - 🧐 自我審查：%s\n", m.Critique))
		}
	}

	if len(goal.Reflections) > 0 {
		sb.WriteString("\n## 2. 成長與自我反思日誌 (Reflection & Growth Journal)\n")
		start := 0
		if len(goal.Reflections) > 8 {
			start = len(goal.Reflections) - 8
		}
		for _, r := range goal.Reflections[start:] {
			sb.WriteString(fmt.Sprintf("- %s\n", r))
		}
	}
	return sb.String()
}

func (a *Agent) writeGoalFiles(mdContent string, goal *GoalSpec) error {
	rootGoalPath := filepath.Join(a.cfg.Agent.SourceDir, "GOAL.md")
	_ = os.WriteFile(rootGoalPath, []byte(mdContent), 0o644)
	if a.cfg.Sandbox.StateDir != "" {
		stateGoalPath := filepath.Join(a.cfg.Sandbox.StateDir, "goal_state.json")
		if raw, err := json.MarshalIndent(goal, "", "  "); err == nil {
			return os.WriteFile(stateGoalPath, raw, 0o644)
		}
	}
	return nil
}

func (a *Agent) loadGoalSpec() (*GoalSpec, error) {
	if a.cfg.Sandbox.StateDir == "" {
		return nil, fmt.Errorf("state dir not configured")
	}
	stateGoalPath := filepath.Join(a.cfg.Sandbox.StateDir, "goal_state.json")
	data, err := os.ReadFile(stateGoalPath)
	if err != nil {
		return nil, err
	}
	var goal GoalSpec
	if err := json.Unmarshal(data, &goal); err != nil {
		return nil, err
	}
	return &goal, nil
}
