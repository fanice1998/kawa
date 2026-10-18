package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fanice1998/kawa/internal/evolution"
	"github.com/fanice1998/kawa/internal/llm"
	"github.com/rs/zerolog/log"
)

// MicroStep represents a single atomic code modification or file creation inside TASK.md.
// Each MicroStep targets ONE file and ONE focused change (15-40 lines) so 14B local models finish in seconds.
type MicroStep struct {
	ID           string   `json:"id"`
	Title        string   `json:"title"`
	TargetFile   string   `json:"target_file"`
	Action       string   `json:"action"` // "modify" or "create"
	FocusSymbols []string `json:"focus_symbols,omitempty"`
	Instruction  string   `json:"instruction"`
	Done         bool     `json:"done"`
}

// TaskSpec represents the structured TASK.md plan with atomic micro-steps and persisted working file checkpoints.
type TaskSpec struct {
	Title        string            `json:"title"`
	Summary      string            `json:"summary"`
	Requirements []string          `json:"requirements"`
	TargetFiles  []string          `json:"target_files"`
	Steps        []MicroStep       `json:"steps"`
	WorkingFiles map[string]string `json:"working_files,omitempty"`
	Stage1Done   bool              `json:"stage1_done"`
	Stage2Done   bool              `json:"stage2_done"`
	Stage3Done   bool              `json:"stage3_done"`
	ProposalID   string            `json:"proposal_id,omitempty"`
	UpdatedAt    string            `json:"updated_at"`
}

// isTaskExecutionTrigger detects when the user confirms a discussed feature and asks Kawa
// to create TASK.md and/or start implementing it autonomously.
func isTaskExecutionTrigger(text string, history []llm.Message) bool {
	cleaned := strings.TrimSpace(text)
	lower := strings.ToLower(cleaned)

	explicitTriggers := []string{
		"就這樣做", "照這樣做", "開始實作", "繼續實作", "繼續開發", "開始開發", "立刻實作",
		"馬上實作", "做成 task.md", "做成task.md", "建立 task.md", "產生 task.md",
		"直接實作", "動手實作", "幫我實作", "請實作", "請開始實作", "確認實作",
	}
	for _, kw := range explicitTriggers {
		if strings.Contains(lower, kw) {
			return true
		}
	}

	shortConfirmations := []string{
		"開始做", "做吧", "開始吧", "好，開始", "好 開始", "沒問題，開始", "確認，開始",
	}
	for _, kw := range shortConfirmations {
		if strings.Contains(lower, kw) && historyDiscussesFeature(history) {
			return true
		}
	}
	return false
}

func historyDiscussesFeature(history []llm.Message) bool {
	featureKeywords := []string{
		"實作", "開發", "新功能", "新增功能", "工具", "tool", "inline", "bot", "修改", "程式碼", "code", "task.md",
	}
	start := 0
	if len(history) > 6 {
		start = len(history) - 6
	}
	for _, m := range history[start:] {
		lower := strings.ToLower(m.Content)
		for _, kw := range featureKeywords {
			if strings.Contains(lower, kw) {
				return true
			}
		}
	}
	return false
}

// loadRelevantDevSkills loads built-in developer skills from `skills/` based on the task context.
func (a *Agent) loadRelevantDevSkills(queryText string, targetFiles []string) string {
	if a.skillMgr == nil {
		return ""
	}
	combined := strings.ToLower(queryText + " " + strings.Join(targetFiles, " "))
	skillNames := []string{"self_improvement"}
	if strings.Contains(combined, "telegram") || strings.Contains(combined, "inline") || strings.Contains(combined, "channel") {
		skillNames = append(skillNames, "telegram_channel_dev")
	}
	if strings.Contains(combined, "tool") || strings.Contains(combined, "工具") || strings.Contains(combined, "custom_tools") {
		skillNames = append(skillNames, "custom_tool_dev")
	}

	var sb strings.Builder
	seen := make(map[string]bool)
	for _, name := range skillNames {
		if seen[name] {
			continue
		}
		seen[name] = true
		if sk, err := a.skillMgr.ReadSkill(name); err == nil && sk != nil {
			sb.WriteString(fmt.Sprintf("### [SKILL: %s]\n%s\n\n", sk.Name, truncateRunes(sk.Content, 2500)))
		}
	}
	return strings.TrimSpace(sb.String())
}

// DraftTaskSpecFromConversation uses Go AST outlines + developer Skills + the active LLM
// to break a confirmed feature into small, atomic MicroSteps in `/Users/fanice/kawa/TASK.md`.
func (a *Agent) DraftTaskSpecFromConversation(ctx context.Context, sessionKey, extraInstruction string) (*TaskSpec, string, error) {
	provider, err := a.llmRegistry.Active()
	if err != nil {
		return nil, "", err
	}

	a.mu.Lock()
	hist := make([]llm.Message, len(a.sessions[sessionKey]))
	copy(hist, a.sessions[sessionKey])
	a.mu.Unlock()

	var convBuilder strings.Builder
	// If the in-memory session was recently reset or restarted, pull recent episodes from persistent RAG memory
	if len(hist) <= 2 && a.memEngine != nil {
		recentItems := a.memEngine.ListRecent(4)
		for i := len(recentItems) - 1; i >= 0; i-- {
			it := recentItems[i]
			if (it.Category == "episodic" || it.Category == "episode") && strings.TrimSpace(it.Content) != "" {
				convBuilder.WriteString(fmt.Sprintf("[近期對話記憶]:\n%s\n\n", truncateRunes(it.Content, 600)))
			}
		}
	}

	start := 0
	if len(hist) > 8 {
		start = len(hist) - 8
	}
	for _, m := range hist[start:] {
		if m.Role == llm.RoleUser || m.Role == llm.RoleAssistant {
			cleanedMsg := strings.TrimSpace(llm.StripThinkingTags(m.Content))
			if cleanedMsg == "" {
				continue
			}
			convBuilder.WriteString(fmt.Sprintf("[%s]: %s\n\n", m.Role, truncateRunes(cleanedMsg, 600)))
		}
	}
	if strings.TrimSpace(extraInstruction) != "" {
		convBuilder.WriteString(fmt.Sprintf("[使用者最新需求/確認指示]: %s\n", extraInstruction))
	}

	// Build dynamic Go AST outlines of core candidate files so the planner knows exact struct/func names
	candidateFiles := []string{
		"internal/channel/telegram.go",
		"internal/channel/channel.go",
		"internal/tools/custom_tools.go",
		"internal/tools/tool.go",
		"internal/agent/agent.go",
	}
	var astOutlines strings.Builder
	for _, rel := range candidateFiles {
		abs := filepath.Join(a.cfg.Agent.SourceDir, filepath.FromSlash(rel))
		if data, err := os.ReadFile(abs); err == nil {
			astOutlines.WriteString(buildGoFileOutline(rel, string(data)) + "\n")
		}
	}

	sysPrompt := `你是 Kawa 系統架構師。請根據【對話紀錄 / 近期對話記憶】與【現有程式碼 Go AST 大綱】，將使用者討論的新功能拆解為 **2~3 個極度細小的原子級微任務 (Micro-Steps)**，並只輸出一個純 JSON 物件（不要輸出任何其他文字或 Markdown）：
{
  "title": "根據對話紀錄整理出的功能名稱",
  "summary": "功能目標與架構設計摘要 (1~2 句話)",
  "requirements": [
    "具體需求 1",
    "具體需求 2"
  ],
  "target_files": [
    "要修改或新增的檔案相對路徑"
  ],
  "steps": [
    {
      "id": "step-1",
      "title": "第一步微任務標題",
      "target_file": "要修改或新增的檔案相對路徑",
      "action": "modify",
      "focus_symbols": ["實際存在於上方 Go AST 大綱中的結構或函式名稱"],
      "instruction": "清楚描述這一步要新增或修改的欄位與方法"
    },
    {
      "id": "step-2",
      "title": "第二步微任務標題",
      "target_file": "要修改或新增的檔案相對路徑",
      "action": "modify",
      "focus_symbols": ["實際存在於上方 Go AST 大綱中的結構或函式名稱"],
      "instruction": "清楚描述這一步要串接的邏輯"
    }
  ]
}
【拆解微任務鐵則（極重要）】：
1. **必須嚴格根據對話紀錄中最近討論的功能來規劃**（絕對禁止無故規劃無關的功能！）。
2. **端到端串接鐵則（End-to-End Wiring）**：凡是讓使用者能在對話中請 Kawa 主動執行的動作（例如：刪除訊息、發送特殊通知、查詢 API、操作檔案等），如果只寫一個底層 struct method，LLM 在對話時是無法呼叫它的！你必須同時規劃在 ` + "`internal/tools/`" + ` 建立對應的 ` + "`Tool`" + ` 並在 ` + "`internal/tools/custom_tools.go`" + ` 的 ` + "`RegisterCustomTools(reg, deps)`" + ` 中註冊（` + "`deps.Router`" + ` 可直接操作通訊通道）！
3. **每個 step 只能修改 1 個檔案中的 1~2 個函式或結構**（每次變更控制在 15~35 行內）。
4. ` + "`focus_symbols`" + ` 請填入上方 Go AST 大綱中實際存在的 symbol 名稱（例如 ` + "`tgUpdatesResp`、`TelegramChannel`、`Start`、`Send`、`RegisterCustomTools`" + `）。
5. **絕對禁止修改 ` + "`internal/tools/tool.go`" + `**。`

	userPrompt := fmt.Sprintf("## 對話紀錄\n%s\n## 現有程式碼 Go AST 大綱\n%s",
		convBuilder.String(), astOutlines.String())

	resp, err := provider.Chat(ctx, llm.ChatRequest{
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: sysPrompt},
			{Role: llm.RoleUser, Content: userPrompt},
		},
	})
	if err != nil {
		return nil, "", fmt.Errorf("產生 TASK.md 規格失敗: %w", err)
	}

	spec := &TaskSpec{
		Title:        "自訂新功能開發任務",
		Summary:      strings.TrimSpace(extraInstruction),
		Requirements: []string{"依據使用者確認之對話需求完成 Golang 實作與沙盒測試"},
		TargetFiles:  []string{"internal/tools/custom_tools.go"},
		UpdatedAt:    time.Now().Format("2006-01-02 15:04:05"),
	}

	if jsonStr := extractFirstJSONObject(resp.Content); jsonStr != "" {
		var parsed TaskSpec
		if err := json.Unmarshal([]byte(jsonStr), &parsed); err == nil && strings.TrimSpace(parsed.Title) != "" {
			spec.Title = parsed.Title
			if parsed.Summary != "" {
				spec.Summary = parsed.Summary
			}
			if len(parsed.Requirements) > 0 {
				spec.Requirements = parsed.Requirements
			}
			if len(parsed.TargetFiles) > 0 {
				spec.TargetFiles = parsed.TargetFiles
			}
			if len(parsed.Steps) > 0 {
				spec.Steps = parsed.Steps
			}
		}
	}

	ensureDefaultMicroSteps(spec)

	mdContent := renderTaskMarkdown(spec)
	if err := a.writeTaskFiles(mdContent, spec); err != nil {
		log.Warn().Err(err).Msg("Failed to persist TASK.md to disk")
	}
	return spec, mdContent, nil
}

// ensureDefaultMicroSteps guarantees that even if a TaskSpec was loaded from an older current_task.json
// without `Steps`, it is automatically decomposed into atomic MicroSteps.
func ensureDefaultMicroSteps(spec *TaskSpec) {
	if len(spec.Steps) > 0 {
		for i := range spec.Steps {
			if spec.Steps[i].ID == "" {
				spec.Steps[i].ID = fmt.Sprintf("step-%d", i+1)
			}
			if spec.Steps[i].Action == "" {
				spec.Steps[i].Action = "modify"
			}
		}
		return
	}

	combined := strings.ToLower(spec.Title + " " + spec.Summary + " " + strings.Join(spec.TargetFiles, " "))
	if strings.Contains(combined, "telegram") && strings.Contains(combined, "inline") {
		spec.TargetFiles = []string{"internal/channel/telegram.go"}
		spec.Steps = []MicroStep{
			{
				ID:           "step-1",
				Title:        "在 tgUpdatesResp 新增 InlineQuery 欄位並實作 answerInlineQuery 方法",
				TargetFile:   "internal/channel/telegram.go",
				Action:       "modify",
				FocusSymbols: []string{"tgUpdatesResp", "sendTypingAction"},
				Instruction:  "在 tgUpdatesResp 的 Result 結構中新增 InlineQuery 欄位 (ID, Query, From)，並在 TelegramChannel 新增 answerInlineQuery(ctx, queryID, queryText) 方法呼叫 Telegram answerInlineQuery API。",
			},
			{
				ID:           "step-2",
				Title:        "在 Start() 輪詢迴圈中處理 InlineQuery 事件",
				TargetFile:   "internal/channel/telegram.go",
				Action:       "modify",
				FocusSymbols: []string{"Start"},
				Instruction:  "在 Start() 的 for _, u := range updates.Result 迴圈中，若 u.InlineQuery != nil 且通過白名單檢查，啟動 goroutine 呼叫 t.answerInlineQuery(ctx, u.InlineQuery.ID, u.InlineQuery.Query)。",
			},
		}
		return
	}

	// Generic decomposition: one MicroStep per target file (excluding read-only tool.go)
	stepIdx := 1
	for _, tf := range spec.TargetFiles {
		clean := filepath.ToSlash(strings.TrimSpace(tf))
		if clean == "" || clean == "internal/tools/tool.go" {
			continue
		}
		spec.Steps = append(spec.Steps, MicroStep{
			ID:          fmt.Sprintf("step-%d", stepIdx),
			Title:       fmt.Sprintf("實作並更新 %s", clean),
			TargetFile:  clean,
			Action:      "modify",
			Instruction: fmt.Sprintf("根據需求 (%s) 完成 %s 的程式碼修改", spec.Summary, clean),
		})
		stepIdx++
	}
}

// RunTaskPipeline executes TASK.md using the Skill-Guided + Atomic Micro-Step Engine:
// 1. Stage 1: Scan target files via Go AST and load relevant developer Skills
// 2. Stage 2: Execute each MicroStep one by one (checkpointing WorkingFiles after each step!)
// 3. Stage 3: Verify in isolated Git Worktree + macOS Seatbelt sandbox (with targeted line-level self-healing)
func (a *Agent) RunTaskPipeline(ctx context.Context, channelName, senderID, extraInstruction string) (string, error) {
	sessionKey := channelName + ":" + senderID

	spec, _ := a.loadTaskSpec()
	if spec == nil || spec.Stage3Done || strings.TrimSpace(extraInstruction) != "" {
		var err error
		spec, _, err = a.DraftTaskSpecFromConversation(ctx, sessionKey, extraInstruction)
		if err != nil {
			return "", err
		}
	}
	ensureDefaultMicroSteps(spec)

	a.notifyProgress(ctx, channelName, senderID, fmt.Sprintf(
		"📋 **已啟動 `TASK.md` 原子微任務開發引擎：%s**\n\n%s\n🚀 共拆分為 **%d 個原子微任務**，現在開始依序執行...",
		spec.Title, renderTaskMarkdown(spec), len(spec.Steps),
	))

	// =========================================================================
	// Stage 1: Inspect AST Outlines, Load Source Files & Developer Skills
	// =========================================================================
	if spec.WorkingFiles == nil {
		spec.WorkingFiles = make(map[string]string)
	}
	for rel, content := range a.inspectTargetFiles(spec.TargetFiles) {
		if _, exists := spec.WorkingFiles[rel]; !exists {
			spec.WorkingFiles[rel] = content
		}
	}
	for _, st := range spec.Steps {
		clean := filepath.ToSlash(strings.TrimSpace(st.TargetFile))
		if clean != "" {
			if _, exists := spec.WorkingFiles[clean]; !exists {
				abs := filepath.Join(a.cfg.Agent.SourceDir, filepath.FromSlash(clean))
				if data, err := os.ReadFile(abs); err == nil {
					spec.WorkingFiles[clean] = string(data)
				}
			}
		}
	}

	skillsGuide := a.loadRelevantDevSkills(spec.Title+" "+spec.Summary, spec.TargetFiles)
	spec.Stage1Done = true
	spec.UpdatedAt = time.Now().Format("2006-01-02 15:04:05")
	_ = a.writeTaskFiles(renderTaskMarkdown(spec), spec)

	// =========================================================================
	// Stage 2: Execute Atomic Micro-Steps One by One (with per-step retry & checkpoint)
	// =========================================================================
	provider, err := a.llmRegistry.Active()
	if err != nil {
		return "", err
	}

	for i := range spec.Steps {
		step := &spec.Steps[i]
		if step.Done {
			continue
		}

		a.notifyProgress(ctx, channelName, senderID, fmt.Sprintf(
			"🔨 **[微任務 %d/%d: `%s`]** 正在執行：**%s** (目標檔案: `%s`)...",
			i+1, len(spec.Steps), step.ID, step.Title, step.TargetFile,
		))

		var stepErr error
		for stepAttempt := 1; stepAttempt <= 3; stepAttempt++ {
			updatedCode, targetPath, err := a.executeSingleMicroStep(ctx, provider, spec, step, skillsGuide, stepErr, stepAttempt)
			if err != nil {
				stepErr = err
				log.Warn().
					Err(err).
					Str("step_id", step.ID).
					Int("attempt", stepAttempt).
					Msg("Atomic micro-step failed; retrying single step")
				continue
			}

			spec.WorkingFiles[targetPath] = updatedCode
			step.Done = true
			spec.UpdatedAt = time.Now().Format("2006-01-02 15:04:05")
			_ = a.writeTaskFiles(renderTaskMarkdown(spec), spec)
			stepErr = nil
			break
		}

		if stepErr != nil {
			return fmt.Sprintf(
				"⚠️ 微任務 **%s (%s)** 在嘗試 3 次後仍未成功：\n```\n%v\n```\n已完成的前置微任務已自動保存於 `.kawa/current_task.json`，可直接輸入 `/run_task` 接續重試！",
				step.ID, step.Title, stepErr,
			), nil
		}

		a.notifyProgress(ctx, channelName, senderID, fmt.Sprintf(
			"✅ **[微任務 %d/%d 完成]** `%s` — **%s** 已寫入暫存區並更新 `TASK.md`！",
			i+1, len(spec.Steps), step.ID, step.Title,
		))
	}

	spec.Stage2Done = true
	spec.UpdatedAt = time.Now().Format("2006-01-02 15:04:05")
	_ = a.writeTaskFiles(renderTaskMarkdown(spec), spec)

	// =========================================================================
	// Stage 3: Sandbox Verification (`go vet`, `go test`, `go build`) + Targeted Self-Healing
	// =========================================================================
	a.notifyProgress(ctx, channelName, senderID,
		"🧪 **[進入 Stage 3 沙盒驗證]** 所有微任務已完成！正在隔離 Git Worktree + macOS Seatbelt 沙盒執行 `go vet ./...`、`go test ./...` 與 `go build ./cmd/kawa`...",
	)

	var finalProposal *evolution.Proposal
	var lastSandboxErr string

	for verifyAttempt := 1; verifyAttempt <= 3; verifyAttempt++ {
		patches := buildPatchesFromWorkingFiles(spec.WorkingFiles)
		proposal, propErr := a.evolveEngine.Propose(ctx, spec.Title, spec.Summary, patches)
		if propErr == nil {
			finalProposal = proposal
			break
		}

		lastSandboxErr = propErr.Error()
		log.Warn().Err(propErr).Int("attempt", verifyAttempt).Msg("Stage 3 sandbox verification failed; running targeted line-level self-heal")

		if verifyAttempt < 3 {
			a.notifyProgress(ctx, channelName, senderID, fmt.Sprintf(
				"🔧 **[Stage 3 局部語法自動修復 %d/3]** 沙盒編譯發現問題，正在擷取報錯行前後 25 行進行快速修復...",
				verifyAttempt,
			))
			if healErr := a.healCompilerError(ctx, provider, spec, lastSandboxErr); healErr != nil {
				log.Warn().Err(healErr).Msg("Targeted compiler error self-heal failed")
			} else {
				_ = a.writeTaskFiles(renderTaskMarkdown(spec), spec)
			}
		}
	}

	if finalProposal == nil {
		return fmt.Sprintf(
			"⚠️ `TASK.md` 開發任務 **%s** 在 Stage 3 沙盒驗證經過 3 次局部修復後仍未通過：\n\n```\n%s\n```\n你可以直接輸入 `/run_task` 再次嘗試修復，或切換模型後重試。",
			spec.Title, truncateRunes(lastSandboxErr, 1200),
		), nil
	}

	spec.Stage3Done = true
	spec.ProposalID = finalProposal.ID
	spec.UpdatedAt = time.Now().Format("2006-01-02 15:04:05")
	_ = a.writeTaskFiles(renderTaskMarkdown(spec), spec)

	if a.memEngine != nil {
		_, _ = a.memEngine.Add("experience", fmt.Sprintf("成功完成開發任務 [%s]: %s (Proposal: %s)", spec.Title, spec.Summary, finalProposal.ID), sessionKey)
	}
	a.recordAction(fmt.Sprintf("Completed TASK.md pipeline -> Proposal %s (%s)", finalProposal.ID, finalProposal.Title))

	diffPreview := finalProposal.Diff
	if len(diffPreview) > 1800 {
		diffPreview = diffPreview[:1800] + "\n...[Diff 已截斷，可輸入 /pending 查看]"
	}

	reply := fmt.Sprintf(
		"🎉 **`TASK.md` 所有階段與微任務已全部執行完畢！**\n\n"+
			"%s\n"+
			"### 🧬 自我進化提案資訊\n"+
			"• **提案 ID**: `%s`\n"+
			"• **功能標題**: %s\n"+
			"• **沙盒驗證**: `go vet ./...`、`go test ./...`、`go build ./cmd/kawa` 全部通過 ✅\n\n"+
			"### 📄 Git Diff 變更預覽\n```diff\n%s\n```\n\n"+
			"👉 **請審核以上變更：**\n"+
			"• 回覆 `/approve %s` 即可自動將程式碼合併至主專案、重新編譯並熱重啟生效！\n"+
			"• 回覆 `/reject %s` 則放棄此次變更。",
		renderTaskMarkdown(spec),
		finalProposal.ID,
		finalProposal.Title,
		diffPreview,
		finalProposal.ID,
		finalProposal.ID,
	)

	a.appendSessionMessage(sessionKey, llm.Message{
		Role:    llm.RoleAssistant,
		Content: reply,
	})
	return reply, nil
}

// executeSingleMicroStep executes ONE atomic MicroStep by slicing only the needed Go AST symbols
// and applying a sanitized SEARCH/REPLACE patch (or creating a new file).
func (a *Agent) executeSingleMicroStep(
	ctx context.Context,
	provider llm.Provider,
	spec *TaskSpec,
	step *MicroStep,
	skillsGuide string,
	prevErr error,
	attempt int,
) (string, string, error) {
	targetPath := filepath.ToSlash(strings.TrimSpace(step.TargetFile))
	if targetPath == "" {
		targetPath = "internal/tools/custom_tools.go"
	}

	existingContent, hasExisting := spec.WorkingFiles[targetPath]
	if !hasExisting {
		abs := filepath.Join(a.cfg.Agent.SourceDir, filepath.FromSlash(targetPath))
		if data, err := os.ReadFile(abs); err == nil {
			existingContent = string(data)
			hasExisting = true
			spec.WorkingFiles[targetPath] = existingContent
		}
	}

	var userPrompt strings.Builder
	userPrompt.WriteString(fmt.Sprintf("## 當前原子微任務 (%s)\n- **標題**: %s\n- **目標檔案**: `%s`\n- **本步具體指示**: %s\n\n",
		step.ID, step.Title, targetPath, step.Instruction))

	if skillsGuide != "" {
		userPrompt.WriteString(fmt.Sprintf("## 參考開發 SKILL 範本\n%s\n\n", skillsGuide))
	}

	if hasExisting && step.Action != "create" {
		sliced := sliceGoFileForSymbols(existingContent, step.FocusSymbols, step.Instruction+" "+step.Title)
		userPrompt.WriteString(fmt.Sprintf("## 目標檔案 `%s` 相關區段原始碼\n```go\n%s\n```\n", targetPath, sliced))
	} else if toolIface, ok := spec.WorkingFiles["internal/tools/tool.go"]; ok {
		userPrompt.WriteString(fmt.Sprintf("## 參考 `internal/tools/tool.go` 介面定義\n```go\n%s\n```\n", toolIface))
	}

	if prevErr != nil {
		userPrompt.WriteString(fmt.Sprintf("\n## ⚠️ 前一次嘗試錯誤 (第 %d 次重試，請修正格式)\n```\n%v\n```\n", attempt-1, prevErr))
	}

	var sysPrompt string
	if hasExisting && step.Action != "create" {
		sysPrompt = `你是 Kawa 的 Golang 微任務開發引擎。現在你只需要專注完成當前這 1 個微任務！
請直接輸出純淨的 SEARCH/REPLACE 區塊（**絕對不要在區塊內加 ` + "```go 或 ```" + ` 符號，也不要輸出教學說明**）：

<<<<<<< SEARCH
(從上方【目標檔案相關區段原始碼】中，100% 原封不動複製要修改的那幾行)
=======
(替換後的新 Go 程式碼)
>>>>>>> REPLACE

【鐵則】：
1. SEARCH 區塊內的每一行必須 100% 從上方【目標檔案相關區段原始碼】原封不動複製（絕對禁止複製 SKILL 範例中的程式碼作為 SEARCH，也不要放 ` + "```go" + `）！
2. 只需要輸出這個微任務要改的那 10~35 行，保持極速精簡！`
	} else {
		sysPrompt = `你是 Kawa 的 Golang 微任務開發引擎。請為目標新檔案輸出完整的 Go 原始碼：
` + "```go\npackage ...\n```"
	}

	resp, err := provider.Chat(ctx, llm.ChatRequest{
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: sysPrompt},
			{Role: llm.RoleUser, Content: userPrompt.String()},
		},
	})
	if err != nil {
		return "", targetPath, err
	}

	// If modifying an existing file, apply SEARCH/REPLACE (with fence sanitization!)
	if hasExisting && step.Action != "create" {
		if strings.Contains(resp.Content, "<<<<<<< SEARCH") && strings.Contains(resp.Content, ">>>>>>> REPLACE") {
			updated, err := applySearchReplaceBlocks(existingContent, resp.Content)
			if err != nil {
				return "", targetPath, err
			}
			return updated, targetPath, nil
		}
		// Fallback if the file was very short (like custom_tools.go) and the model returned a full valid file
		code := extractGoCodeFromSection(resp.Content)
		if len(strings.Split(existingContent, "\n")) <= 50 && strings.Contains(code, "package ") && strings.Contains(code, "func ") {
			return code, targetPath, nil
		}
		return "", targetPath, fmt.Errorf("未輸出有效的 <<<<<<< SEARCH ... ======= ... >>>>>>> REPLACE 區塊")
	}

	// Creating a new file
	code := extractGoCodeFromSection(resp.Content)
	if !strings.Contains(code, "package ") {
		return "", targetPath, fmt.Errorf("新檔案 %s 缺少有效的 package 宣告", targetPath)
	}
	return code, targetPath, nil
}

// healCompilerError extracts +/- 25 lines around the exact compiler error line and asks the LLM for a quick SEARCH/REPLACE fix.
func (a *Agent) healCompilerError(ctx context.Context, provider llm.Provider, spec *TaskSpec, compilerErr string) error {
	errFile, snippet := extractCompilerErrorSnippet(compilerErr, spec.WorkingFiles)
	if errFile == "" {
		return fmt.Errorf("compiler error did not reference any modified file in working_files")
	}

	sysPrompt := `你是 Kawa 的 Golang 編譯錯誤極速修復器。請根據編譯器報錯與局部程式碼，只輸出一個純淨的 SEARCH/REPLACE 區塊修復該錯誤（不要在內部放 ` + "```go" + `）：
<<<<<<< SEARCH
(原檔案中有問題的那幾行)
=======
(修復後的正確 Go 程式碼)
>>>>>>> REPLACE`

	userPrompt := fmt.Sprintf("## 編譯器報錯\n```\n%s\n```\n\n## 檔案 `%s` 報錯處局部程式碼\n```go\n%s\n```",
		truncateRunes(compilerErr, 1200), errFile, snippet)

	resp, err := provider.Chat(ctx, llm.ChatRequest{
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: sysPrompt},
			{Role: llm.RoleUser, Content: userPrompt},
		},
	})
	if err != nil {
		return err
	}

	updated, err := applySearchReplaceBlocks(spec.WorkingFiles[errFile], resp.Content)
	if err != nil {
		return err
	}
	spec.WorkingFiles[errFile] = updated
	return nil
}

func buildPatchesFromWorkingFiles(workingFiles map[string]string) []evolution.FilePatch {
	var patches []evolution.FilePatch
	for path, content := range workingFiles {
		if path == "" || path == "internal/tools/tool.go" {
			continue
		}
		patches = append(patches, evolution.FilePatch{
			Path:    path,
			Content: content,
		})
	}
	return patches
}

func (a *Agent) inspectTargetFiles(paths []string) map[string]string {
	result := make(map[string]string)
	var all []string
	for _, p := range paths {
		clean := filepath.ToSlash(strings.TrimSpace(p))
		if clean != "" {
			all = append(all, clean)
		}
	}
	if len(all) == 0 {
		all = []string{"internal/tools/custom_tools.go"}
	}

	seen := make(map[string]bool)
	for _, rel := range all {
		if seen[rel] {
			continue
		}
		seen[rel] = true
		abs := filepath.Join(a.cfg.Agent.SourceDir, filepath.FromSlash(rel))
		data, err := os.ReadFile(abs)
		if err == nil {
			result[rel] = string(data)
		}
	}
	return result
}

type llmPatchResponse struct {
	Title       string                `json:"title"`
	Description string                `json:"description"`
	Files       []evolution.FilePatch `json:"files"`
}

// parseTaskPatchesFromResponse parses `### FILE: <path>` blocks (supporting both SEARCH/REPLACE diffs
// and full ```go code blocks), with a fallback to JSON `{"files": [...]}`.
func parseTaskPatchesFromResponse(raw string, existingFiles map[string]string) ([]evolution.FilePatch, error) {
	lines := strings.Split(raw, "\n")
	type fileSection struct {
		path string
		body string
	}
	var sections []fileSection
	var curPath string
	var curBuf strings.Builder

	flushSection := func() {
		if curPath != "" {
			sections = append(sections, fileSection{
				path: curPath,
				body: curBuf.String(),
			})
		}
		curPath = ""
		curBuf.Reset()
	}

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "### FILE:") || strings.HasPrefix(trimmed, "### MODIFY:") || strings.HasPrefix(trimmed, "### CREATE:") {
			flushSection()
			parts := strings.SplitN(trimmed, ":", 2)
			if len(parts) == 2 {
				p := strings.TrimSpace(parts[1])
				p = strings.Trim(p, "`*\"' ")
				curPath = filepath.ToSlash(p)
			}
			continue
		}
		if curPath != "" {
			curBuf.WriteString(line + "\n")
		}
	}
	flushSection()

	if len(sections) > 0 {
		patchMap := make(map[string]string)
		var order []string
		for _, sec := range sections {
			if sec.path == "" || sec.path == "internal/tools/tool.go" {
				continue
			}
			baseContent, hasExisting := patchMap[sec.path]
			if !hasExisting {
				baseContent, hasExisting = existingFiles[sec.path]
			}

			if strings.Contains(sec.body, "<<<<<<< SEARCH") && strings.Contains(sec.body, ">>>>>>> REPLACE") {
				if !hasExisting {
					return nil, fmt.Errorf("檔案 %s 不存在於現有上下文中，無法套用 SEARCH/REPLACE", sec.path)
				}
				updated, err := applySearchReplaceBlocks(baseContent, sec.body)
				if err != nil {
					return nil, fmt.Errorf("套用 %s 的 SEARCH/REPLACE 失敗: %w", sec.path, err)
				}
				if _, seen := patchMap[sec.path]; !seen {
					order = append(order, sec.path)
				}
				patchMap[sec.path] = updated
				continue
			}

			code := extractGoCodeFromSection(sec.body)
			if strings.Contains(code, "package ") {
				if _, seen := patchMap[sec.path]; !seen {
					order = append(order, sec.path)
				}
				patchMap[sec.path] = code
			}
		}

		var out []evolution.FilePatch
		for _, p := range order {
			out = append(out, evolution.FilePatch{
				Path:    p,
				Content: patchMap[p],
			})
		}
		if len(out) > 0 {
			return out, nil
		}
	}

	if jsonStr := extractFirstJSONObject(raw); jsonStr != "" {
		var parsed llmPatchResponse
		if err := json.Unmarshal([]byte(jsonStr), &parsed); err == nil && len(parsed.Files) > 0 {
			return parsed.Files, nil
		}
	}

	return nil, fmt.Errorf("模型未輸出有效的 `### FILE: <path>` 或程式碼區塊 (輸出預覽: %s)", truncateRunes(raw, 240))
}

// sanitizePatchBlock strips markdown code fences (```go, ```) and comment headers that 14B models
// frequently accidentally place inside <<<<<<< SEARCH or >>>>>>> REPLACE blocks.
func sanitizePatchBlock(block string) string {
	lines := strings.Split(block, "\n")
	var cleaned []string
	for _, line := range lines {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "```") || strings.HasPrefix(t, "// --- [") {
			continue
		}
		cleaned = append(cleaned, line)
	}
	res := strings.Join(cleaned, "\n")
	res = strings.TrimPrefix(res, "\n")
	res = strings.TrimSuffix(res, "\n")
	res = strings.TrimSuffix(res, "\r")
	return res
}

func applySearchReplaceBlocks(original, body string) (string, error) {
	result := original
	rem := body
	applied := 0

	for {
		sIdx := strings.Index(rem, "<<<<<<< SEARCH")
		if sIdx == -1 {
			break
		}
		rem = rem[sIdx+len("<<<<<<< SEARCH"):]
		if strings.HasPrefix(rem, "\r\n") {
			rem = rem[2:]
		} else if strings.HasPrefix(rem, "\n") {
			rem = rem[1:]
		}

		divIdx := strings.Index(rem, "=======")
		if divIdx == -1 {
			return "", fmt.Errorf("缺少 ======= 分隔線")
		}
		searchBlock := sanitizePatchBlock(rem[:divIdx])

		rem = rem[divIdx+len("======="):]
		if strings.HasPrefix(rem, "\r\n") {
			rem = rem[2:]
		} else if strings.HasPrefix(rem, "\n") {
			rem = rem[1:]
		}

		eIdx := strings.Index(rem, ">>>>>>> REPLACE")
		if eIdx == -1 {
			return "", fmt.Errorf("缺少 >>>>>>> REPLACE 結束標記")
		}
		replaceBlock := sanitizePatchBlock(rem[:eIdx])
		rem = rem[eIdx+len(">>>>>>> REPLACE"):]

		if searchBlock == "" {
			continue
		}

		if strings.Contains(result, searchBlock) {
			result = strings.Replace(result, searchBlock, replaceBlock, 1)
			applied++
			continue
		}
		if updated, ok := replaceTrimmedLines(result, searchBlock, replaceBlock); ok {
			result = updated
			applied++
			continue
		}
		return "", fmt.Errorf("在原檔案中找不到對應的 SEARCH 區塊:\n%s", truncateRunes(searchBlock, 200))
	}

	if applied == 0 {
		return "", fmt.Errorf("未找到任何有效的 SEARCH/REPLACE 區塊")
	}
	return result, nil
}

func replaceTrimmedLines(original, searchBlock, replaceBlock string) (string, bool) {
	origLines := strings.Split(original, "\n")
	var searchLines []string
	for _, sl := range strings.Split(strings.TrimSpace(searchBlock), "\n") {
		if strings.TrimSpace(sl) != "" {
			searchLines = append(searchLines, sl)
		}
	}
	if len(searchLines) == 0 {
		return "", false
	}

	// First try exact line-by-line trimmed match (preserving empty lines in original)
	rawSearchLines := strings.Split(strings.TrimSpace(searchBlock), "\n")
	for i := 0; i <= len(origLines)-len(rawSearchLines); i++ {
		match := true
		for j := 0; j < len(rawSearchLines); j++ {
			if strings.TrimSpace(origLines[i+j]) != strings.TrimSpace(rawSearchLines[j]) {
				match = false
				break
			}
		}
		if match {
			var out []string
			out = append(out, origLines[:i]...)
			out = append(out, replaceBlock)
			out = append(out, origLines[i+len(rawSearchLines):]...)
			return strings.Join(out, "\n"), true
		}
	}
	return "", false
}

func extractGoCodeFromSection(body string) string {
	lines := strings.Split(body, "\n")
	var inFence bool
	var buf strings.Builder
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			if !inFence {
				inFence = true
				continue
			}
			break
		}
		if inFence {
			buf.WriteString(line + "\n")
		}
	}
	if out := strings.TrimSpace(buf.String()); out != "" {
		return out + "\n"
	}
	return strings.TrimSpace(body) + "\n"
}

func renderTaskMarkdown(spec *TaskSpec) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("# 📋 TASK.md — %s\n\n", spec.Title))
	sb.WriteString(fmt.Sprintf("> **最後更新時間**: `%s`\n\n", spec.UpdatedAt))
	sb.WriteString("## 1. 功能目標摘要\n")
	sb.WriteString(spec.Summary + "\n\n")
	sb.WriteString("## 2. 確認後之具體需求\n")
	for _, req := range spec.Requirements {
		sb.WriteString(fmt.Sprintf("- %s\n", req))
	}
	sb.WriteString("\n## 3. 目標檢視與修改檔案\n")
	for _, f := range spec.TargetFiles {
		sb.WriteString(fmt.Sprintf("- `%s`\n", f))
	}
	sb.WriteString("\n## 4. 自主開發執行階段 (Autonomous Execution Stages)\n")
	sb.WriteString(fmt.Sprintf("- [%s] **Stage 1**: 載入開發 Skill 並透過 Go AST 掃描檔案結構大綱\n", checkbox(spec.Stage1Done)))
	sb.WriteString(fmt.Sprintf("- [%s] **Stage 2**: 逐一執行原子級微任務 (Atomic Micro-Steps)\n", checkbox(spec.Stage2Done)))
	for _, st := range spec.Steps {
		sb.WriteString(fmt.Sprintf("  - [%s] `%s` (%s): %s\n", checkbox(st.Done), st.ID, st.TargetFile, st.Title))
	}
	sb.WriteString(fmt.Sprintf("- [%s] **Stage 3**: 隔離 Git Worktree + macOS Seatbelt 沙盒驗證 (`go vet` / `go test` / `go build`)\n", checkbox(spec.Stage3Done)))
	if spec.ProposalID != "" {
		sb.WriteString(fmt.Sprintf("\n## 5. 產出提案\n- **Proposal ID**: `%s` (等待 `/approve %s`)\n", spec.ProposalID, spec.ProposalID))
	}
	return sb.String()
}

func checkbox(done bool) string {
	if done {
		return "x"
	}
	return " "
}

func (a *Agent) writeTaskFiles(mdContent string, spec *TaskSpec) error {
	rootTaskPath := filepath.Join(a.cfg.Agent.SourceDir, "TASK.md")
	if err := os.WriteFile(rootTaskPath, []byte(mdContent), 0o644); err != nil {
		return err
	}
	if a.cfg.Agent.WorkspaceDir != "" && a.cfg.Agent.WorkspaceDir != a.cfg.Agent.SourceDir {
		wsTaskPath := filepath.Join(a.cfg.Agent.WorkspaceDir, "TASK.md")
		_ = os.WriteFile(wsTaskPath, []byte(mdContent), 0o644)
	}
	if a.cfg.Sandbox.StateDir != "" {
		stateTaskPath := filepath.Join(a.cfg.Sandbox.StateDir, "current_task.json")
		if raw, err := json.MarshalIndent(spec, "", "  "); err == nil {
			_ = os.WriteFile(stateTaskPath, raw, 0o644)
		}
	}
	return nil
}

func (a *Agent) loadTaskSpec() (*TaskSpec, error) {
	if a.cfg.Sandbox.StateDir == "" {
		return nil, fmt.Errorf("state dir not configured")
	}
	stateTaskPath := filepath.Join(a.cfg.Sandbox.StateDir, "current_task.json")
	data, err := os.ReadFile(stateTaskPath)
	if err != nil {
		return nil, err
	}
	var spec TaskSpec
	if err := json.Unmarshal(data, &spec); err != nil {
		return nil, err
	}
	return &spec, nil
}

func (a *Agent) notifyProgress(ctx context.Context, channelName, senderID, msg string) {
	if a.router == nil || channelName == "" || senderID == "" {
		return
	}
	_ = a.router.Send(ctx, channelName, senderID, msg)
}

// extractFirstJSONObject finds the first balanced `{ ... }` JSON object in text,
// ignoring surrounding markdown code fences or prose.
func extractFirstJSONObject(text string) string {
	for start := 0; start < len(text); start++ {
		if text[start] != '{' {
			continue
		}
		depth := 0
		inString := false
		escaped := false
		for i := start; i < len(text); i++ {
			ch := text[i]
			if inString {
				if escaped {
					escaped = false
				} else if ch == '\\' {
					escaped = true
				} else if ch == '"' {
					inString = false
				}
				continue
			}
			switch ch {
			case '"':
				inString = true
			case '{':
				depth++
			case '}':
				depth--
				if depth == 0 {
					candidate := text[start : i+1]
					var js map[string]any
					if json.Unmarshal([]byte(candidate), &js) == nil {
						return candidate
					}
					break
				}
			}
		}
	}
	return ""
}

func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "\n...[truncated]"
}
