package agent

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/fanice1998/kawa/internal/channel"
	"github.com/fanice1998/kawa/internal/config"
	"github.com/fanice1998/kawa/internal/evolution"
	"github.com/fanice1998/kawa/internal/llm"
	"github.com/fanice1998/kawa/internal/memory"
	"github.com/fanice1998/kawa/internal/scheduler"
	"github.com/fanice1998/kawa/internal/skills"
	"github.com/fanice1998/kawa/internal/tools"
	"github.com/rs/zerolog/log"
)

// Agent coordinates LLM providers, sandboxed tools, RAG memory, scheduler, self-evolution, and communication channels.
type Agent struct {
	cfg          *config.Config
	llmRegistry  *llm.Registry
	toolRegistry *tools.Registry
	skillMgr     *skills.Manager
	memEngine    *memory.Engine
	sched        *scheduler.Scheduler
	evolveEngine *evolution.Engine
	router       *channel.Router
	onHotReload  func()

	mu            sync.Mutex
	sessions      map[string][]llm.Message
	recentActions []string

	queueMu      sync.Mutex
	pendingQueue []channel.InboundMessage
	activeTask   *ActiveTaskInfo
	queueNotify  chan struct{}
}

func New(
	cfg *config.Config,
	llmRegistry *llm.Registry,
	toolRegistry *tools.Registry,
	skillMgr *skills.Manager,
	memEngine *memory.Engine,
	sched *scheduler.Scheduler,
	evolveEngine *evolution.Engine,
	router *channel.Router,
	onHotReload func(),
) *Agent {
	a := &Agent{
		cfg:          cfg,
		llmRegistry:  llmRegistry,
		toolRegistry: toolRegistry,
		skillMgr:     skillMgr,
		memEngine:    memEngine,
		sched:        sched,
		evolveEngine: evolveEngine,
		router:       router,
		onHotReload:  onHotReload,
		sessions:     make(map[string][]llm.Message),
		queueNotify:  make(chan struct{}, 1),
	}
	a.recordAction("Agent initialized and ready")
	return a
}

// Run processes incoming messages and starts the autonomous scheduler until ctx is cancelled.
func (a *Agent) Run(ctx context.Context) error {
	a.router.StartAll(ctx)
	go a.checkAndSendRebootNotice(ctx)
	go a.runQueueWorker(ctx)
	if a.sched != nil {
		a.sched.SetTriggerHandler(a.handleScheduledTrigger)
		go a.sched.Start(ctx)
	}
	log.Info().Str("agent", a.cfg.Agent.Name).Msg("Kawa Agent main loop started")

	for {
		select {
		case <-ctx.Done():
			log.Info().Msg("Shutting down Kawa Agent loop")
			return nil
		case msg := <-a.router.Inbound():
			a.enqueueOrExecuteInbound(ctx, msg)
		}
	}
}

func (a *Agent) handleScheduledTrigger(ctx context.Context, job scheduler.Job) {
	a.recordAction(fmt.Sprintf("Triggered scheduled job %s (%s)", job.ID, job.Name))

	if job.Mode == scheduler.ModeDirectMessage {
		text := fmt.Sprintf("⏰ **[排程提醒: %s]**\n%s", job.Name, job.Prompt)
		if err := a.router.Send(ctx, job.Channel, job.Recipient, text); err != nil {
			log.Error().Err(err).Str("job_id", job.ID).Msg("Failed to send scheduled direct message")
		}
		return
	}

	trimmedPrompt := strings.TrimSpace(job.Prompt)
	var taskPrompt string
	if strings.HasPrefix(trimmedPrompt, "/") {
		taskPrompt = trimmedPrompt
	} else {
		taskPrompt = fmt.Sprintf("[系統排程自動喚醒任務: %s]\n請立即自主呼叫所需工具完成以下排程任務，並將結果整理回報：\n%s", job.Name, job.Prompt)
	}
	reply, err := a.ProcessMessage(ctx, job.Channel, job.Recipient, taskPrompt)
	if err != nil {
		reply = fmt.Sprintf("⚠️ 排程任務 `%s` 執行錯誤: %v", job.Name, err)
	} else {
		reply = fmt.Sprintf("⏰ **[排程任務完成: %s]**\n%s", job.Name, reply)
	}
	if err := a.router.Send(ctx, job.Channel, job.Recipient, reply); err != nil {
		log.Error().Err(err).Str("job_id", job.ID).Msg("Failed to deliver scheduled agent task output")
	}
}

func (a *Agent) handleInbound(ctx context.Context, msg channel.InboundMessage) {
	log.Info().
		Str("channel", msg.ChannelName).
		Str("sender", msg.SenderID).
		Int("message_id", msg.MessageID).
		Str("text", truncateLog(msg.Text, 120)).
		Msg("Received inbound message")

	a.recordAction(fmt.Sprintf("Received message from %s:%s (%s)", msg.ChannelName, msg.SenderID, truncateLog(msg.Text, 40)))

	ctx = tools.WithMessageMeta(ctx, msg.MessageID, msg.ReplyToMessageID)

	// Keep "typing..." status continuously active every 4s until ProcessMessage finishes
	stopTyping := a.router.StartTyping(ctx, msg.ChannelName, msg.SenderID)
	reply, err := a.ProcessMessage(ctx, msg.ChannelName, msg.SenderID, msg.Text)
	stopTyping()

	if err != nil {
		log.Error().Err(err).Str("channel", msg.ChannelName).Msg("Failed to process message")
		reply = fmt.Sprintf("⚠️ 執行發生錯誤: %v", err)
	}

	if err := a.router.Send(ctx, msg.ChannelName, msg.SenderID, reply); err != nil {
		log.Error().Err(err).Str("channel", msg.ChannelName).Str("sender", msg.SenderID).Msg("Failed to send reply")
	}
}

// ProcessMessage handles slash commands or executes the Hermes-inspired ReAct + RAG tool-calling loop.
func (a *Agent) ProcessMessage(ctx context.Context, channelName, senderID, text string) (string, error) {
	trimmed := strings.TrimSpace(text)
	if strings.HasPrefix(trimmed, "/") {
		if cmdReply, handled := a.handleCommand(ctx, channelName, senderID, trimmed); handled {
			return cmdReply, nil
		}
	}

	ctx = tools.WithDeliveryTarget(ctx, channelName, senderID)

	sessionKey := channelName + ":" + senderID
	history := a.appendSessionMessage(sessionKey, llm.Message{
		Role:    llm.RoleUser,
		Content: trimmed,
	})

	// Multi-feature requests ("分開製作以下功能 1. A 2. B ...") are queued as sequential milestones
	// so they are built strictly one at a time instead of being merged into a single huge task.
	if header, items, ok := parseBatchFeatures(trimmed); ok {
		return a.startFeatureBatch(ctx, channelName, senderID, header, items), nil
	}

	// Autonomous TASK.md Pipeline Trigger:
	// When the user confirms a discussed feature (e.g., "就這樣做！", "開始實作", "做成 TASK.md"),
	// automatically generate TASK.md and run Stage 1 -> Stage 2 -> Stage 3 to completion!
	if isTaskExecutionTrigger(trimmed, history) {
		log.Info().
			Str("channel", channelName).
			Str("trigger", truncateLog(trimmed, 80)).
			Msg("Triggered autonomous TASK.md multi-stage feature development pipeline")
		return a.RunTaskPipeline(ctx, channelName, senderID, trimmed)
	}

	provider, err := a.llmRegistry.Active()
	if err != nil {
		return "", err
	}

	maxIter := a.cfg.Agent.MaxIterations
	if maxIter <= 0 {
		maxIter = 15
	}

	toolDefs := a.toolRegistry.Definitions()
	if isCasualGreeting(trimmed) {
		toolDefs = nil
	}

	sysMsg := llm.Message{
		Role:    llm.RoleSystem,
		Content: a.buildSystemPrompt(channelName, senderID, trimmed, toolDefs),
	}

	executedCalls := make(map[string]bool)
	nudgeCount := 0

	for iter := 1; iter <= maxIter; iter++ {
		reqMsgs := make([]llm.Message, 0, len(history)+1)
		reqMsgs = append(reqMsgs, sysMsg)
		for i, hm := range history {
			// Keep the last 4 messages in full; cap older historical messages to 400 runes
			// so a long response in an earlier turn never slows down future turns on local LLMs.
			if i < len(history)-4 && hm.Role == llm.RoleAssistant && len([]rune(hm.Content)) > 400 {
				r := []rune(hm.Content)
				hm.Content = string(r[:400]) + "...(歷史長回覆已精簡)"
			}
			reqMsgs = append(reqMsgs, hm)
		}

		log.Info().
			Str("provider", provider.Name()).
			Str("model", provider.Model()).
			Int("iteration", iter).
			Int("tools_offered", len(toolDefs)).
			Msg("Invoking LLM provider")

		resp, err := provider.Chat(ctx, llm.ChatRequest{
			Messages: reqMsgs,
			Tools:    toolDefs,
		})
		if err != nil {
			return "", fmt.Errorf("LLM (%s) error: %w", provider.Name(), err)
		}

		log.Info().
			Str("provider", provider.Name()).
			Int("iteration", iter).
			Int("tool_calls", len(resp.ToolCalls)).
			Str("content_preview", truncateLog(resp.Content, 120)).
			Msg("LLM response received")

		// Auto-Correction Guard:
		// 1) Model lazily tells the user "你可以使用 list_dir" instead of calling it itself, OR
		// 2) Model outputs a truncated/promised tool call like "好的，我將會查詢...\n```" without an actual ToolCall!
		if len(resp.ToolCalls) == 0 && len(toolDefs) > 0 && nudgeCount < 3 {
			if mentionedTool := mentionsToolWithoutCalling(resp.Content, toolDefs); mentionedTool != "" {
				nudgeCount++
				log.Warn().
					Str("mentioned_tool", mentionedTool).
					Msg("Intercepted lazy tool mention; forcing autonomous tool execution")

				history = a.appendSessionMessage(sessionKey, llm.Message{
					Role:    llm.RoleAssistant,
					Content: resp.Content,
				})
				nudge := fmt.Sprintf(
					"[System Autonomous Correction] 你剛剛提到了工具 `%s` 但沒有實際呼叫它！請不要使用 ``` 區塊，現在立刻直接輸出純 JSON 執行工具：\n{\"name\": %q, \"arguments\": {}}",
					mentionedTool, mentionedTool,
				)
				history = a.appendSessionMessage(sessionKey, llm.Message{
					Role:    llm.RoleUser,
					Content: nudge,
				})
				continue
			}

			if isIncompleteToolAttempt(resp.Content) || isFalseCapabilityRefusal(resp.Content) {
				nudgeCount++
				log.Warn().
					Str("content", truncateLog(resp.Content, 100)).
					Msg("Intercepted incomplete or false-refusal response; forcing clean JSON tool call")

				history = a.appendSessionMessage(sessionKey, llm.Message{
					Role:    llm.RoleAssistant,
					Content: resp.Content,
				})
				nudge := fmt.Sprintf(
					"[System Autonomous Correction] 你上一則回覆只宣告了動作（或以冒號/刪節號結尾）卻沒有真正執行，對話因此中斷。請針對使用者的需求 (%q) 立刻繼續：若需要資訊或操作，直接呼叫對應工具（`read_file`、`list_dir`、`web_search`、`delete_message` 等）；若資訊已足夠，直接給出完整的最終回答或實作計畫。不要再只說「讓我先…」。",
					trimmed,
				)
				history = a.appendSessionMessage(sessionKey, llm.Message{
					Role:    llm.RoleUser,
					Content: nudge,
				})
				continue
			}
		}

		// Prevent duplicate identical tool calls in the same turn
		var freshToolCalls []llm.ToolCall
		for _, tc := range resp.ToolCalls {
			sig := tc.Name + ":" + strings.TrimSpace(tc.Arguments)
			if executedCalls[sig] {
				log.Warn().Str("tool", tc.Name).Msg("Prevented duplicate identical tool call in same turn; requesting natural language summary")
				continue
			}
			executedCalls[sig] = true
			freshToolCalls = append(freshToolCalls, tc)
		}

		if len(resp.ToolCalls) > 0 && len(freshToolCalls) == 0 {
			toolDefs = nil
			continue
		}
		resp.ToolCalls = freshToolCalls

		assistantMsg := llm.Message{
			Role:      llm.RoleAssistant,
			Content:   resp.Content,
			ToolCalls: resp.ToolCalls,
		}
		history = a.appendSessionMessage(sessionKey, assistantMsg)

		if len(resp.ToolCalls) == 0 {
			finalReply := strings.TrimSpace(resp.Content)
			if finalReply == "" {
				finalReply = "(模型未回傳文字內容)"
			}
			if isIncompleteToolAttempt(finalReply) {
				finalReply += "\n\n⚠️ 模型在多次自動催促後仍中斷於此，沒有給出後續。請直接回覆「繼續」，或輸入 `/reset` 後重新描述需求（建議一次只做一個功能）。"
			}
			if a.memEngine != nil && !isCasualGreeting(trimmed) {
				// If user explicitly asked Kawa to remember a fact/identity and the LLM didn't call memory_save,
				// automatically persist it to Core Memory (MEMORY.md) + RAG!
				if !executedCalls["memory_save"] && shouldAutoSaveCoreMemory(trimmed) {
					_, _ = a.memEngine.Add("core", trimmed, sessionKey)
					log.Info().Str("fact", truncateLog(trimmed, 80)).Msg("Auto-saved user fact to Core Memory (MEMORY.md)")
				}
				a.memEngine.RecordEpisode(sessionKey, trimmed, finalReply)
			}
			return finalReply, nil
		}

		for _, tc := range resp.ToolCalls {
			if tc.Name == "memory_save" {
				executedCalls["memory_save"] = true
			}
			log.Info().
				Str("tool", tc.Name).
				Str("call_id", tc.ID).
				Str("args", truncateLog(tc.Arguments, 160)).
				Msg("Executing sandboxed tool")

			a.recordAction(fmt.Sprintf("Executed tool %s(%s)", tc.Name, truncateLog(tc.Arguments, 60)))

			toolOutput := a.executeToolSafe(ctx, tc.Name, tc.Arguments)
			history = a.appendSessionMessage(sessionKey, llm.Message{
				Role:       llm.RoleTool,
				ToolCallID: tc.ID,
				ToolName:   tc.Name,
				Content:    toolOutput,
			})
		}
	}

	return "⚠️ 已達到單次對話最大工具呼叫次數上限 (max_iterations)。", nil
}

func shouldAutoSaveCoreMemory(userText string) bool {
	lower := strings.ToLower(strings.TrimSpace(userText))
	// Explicit "please remember" requests should always be saved
	for _, explicit := range []string{"請記住", "幫我記住", "請記得", "幫我記下", "remember that"} {
		if strings.Contains(lower, explicit) {
			return true
		}
	}
	// Do not save questions like "你記得我是誰嗎？" as facts
	if strings.Contains(lower, "?") || strings.Contains(lower, "？") || strings.Contains(lower, "嗎") || strings.Contains(lower, "誰") {
		return false
	}
	for _, kw := range []string{"記住", "我是 ", "我是", "我的名字是", "我叫", "我喜歡", "my name is"} {
		if strings.Contains(lower, kw) {
			return true
		}
	}
	return false
}

// isIncompleteToolAttempt detects when a model says "好的，我將會查詢..." or role-plays "(正在呼叫...)"
// without emitting an actual ToolCall.
func isIncompleteToolAttempt(content string) bool {
	trimmed := strings.TrimSpace(content)
	if strings.HasSuffix(trimmed, "```json") || strings.HasSuffix(trimmed, "```xml") || strings.Contains(trimmed, "<tool_call") {
		return true
	}
	if strings.HasSuffix(trimmed, "```") && strings.Count(trimmed, "```")%2 == 1 {
		return true
	}
	// Detect "announce-then-stop": the reply ends with a colon / ellipsis promising a next action
	// (e.g. "讓我先看一下 custom_tools.go 的結構：") but never emitted a tool call.
	for _, suffix := range []string{"：", ":", "…", "..."} {
		if strings.HasSuffix(trimmed, suffix) {
			return true
		}
	}
	// Detect role-played tool execution in parentheses like "（正在呼叫 Telegram...）"
	for _, rp := range []string{"（正在呼叫", "(正在呼叫", "我現在來刪除你的訊息", "正在為您刪除"} {
		if strings.Contains(trimmed, rp) {
			return true
		}
	}
	// If it's a very short reply promising to search/check without any actual results
	if len([]rune(trimmed)) < 60 {
		promisePhrases := []string{
			"我將會查詢", "我將查詢", "我會查詢", "我來查詢", "正在查詢",
			"我將會搜尋", "我來搜尋", "正在搜尋", "讓我查詢", "讓我幫你查",
			"我來查看", "我將查看", "正在為您查詢",
		}
		for _, p := range promisePhrases {
			if strings.Contains(trimmed, p) {
				return true
			}
		}
	}
	return false
}

// isFalseCapabilityRefusal detects when a local model mistakenly claims it cannot search the web or read files.
func isFalseCapabilityRefusal(content string) bool {
	lower := strings.ToLower(content)
	refusals := []string{
		"無法執行網路查詢", "無法上網", "沒有網路查詢", "無法搜尋網路",
		"無法訪問網路", "無法連線到網路", "無法查詢即時", "我沒有上網",
		"cannot browse the web", "cannot search the internet", "don't have internet access",
	}
	for _, r := range refusals {
		if strings.Contains(lower, r) {
			return true
		}
	}
	return false
}

// mentionsToolWithoutCalling checks if the assistant response lazily instructs the user to call a tool
// (e.g. "你可以使用 list_dir 工具") instead of invoking it autonomously.
func mentionsToolWithoutCalling(content string, defs []llm.ToolDefinition) string {
	lower := strings.ToLower(content)
	lazyHints := []string{
		"你可以使用", "可以調用", "可以呼叫", "請使用", "建議使用", "透過呼叫", "通過調用",
		"you can use", "you can call", "please use", "try calling", "use the ",
	}
	hasLazyHint := false
	for _, h := range lazyHints {
		if strings.Contains(lower, h) {
			hasLazyHint = true
			break
		}
	}
	if !hasLazyHint {
		return ""
	}
	for _, d := range defs {
		if strings.Contains(lower, d.Name) {
			return d.Name
		}
	}
	return ""
}

// isCasualGreeting returns true ONLY for exact short greetings ("你好", "hi") that don't ask any question about state or actions.
func isCasualGreeting(text string) bool {
	cleaned := strings.ToLower(strings.TrimSpace(text))
	cleaned = strings.Trim(cleaned, "!！?？.。~～,， ")
	casualPhrases := map[string]bool{
		"hi": true, "hello": true, "hey": true, "yo": true,
		"你好": true, "你好呀": true, "你好啊": true, "哈囉": true, "嗨": true,
		"早安": true, "午安": true, "晚安": true, "安安": true,
		"謝謝": true, "感謝": true, "ok": true, "好": true,
	}
	return casualPhrases[cleaned]
}

func (a *Agent) executeToolSafe(ctx context.Context, name, rawArgs string) string {
	t, ok := a.toolRegistry.Get(name)
	if !ok {
		return fmt.Sprintf("Error: unknown tool %q", name)
	}
	out, err := t.Execute(ctx, rawArgs)
	if err != nil {
		log.Warn().Err(err).Str("tool", name).Msg("Tool execution returned error")
		return fmt.Sprintf("Tool %q error: %v", name, err)
	}
	return out
}

func (a *Agent) handleCommand(ctx context.Context, channelName, senderID, cmdLine string) (string, bool) {
	fields := strings.Fields(cmdLine)
	if len(fields) == 0 {
		return "", false
	}
	cmd := strings.ToLower(fields[0])
	// Support Telegram group commands like `/help@fanice_kawa_bot`
	if idx := strings.Index(cmd, "@"); idx != -1 {
		cmd = cmd[:idx]
	}

	switch cmd {
	case "/help":
		return strings.Join([]string{
			"🤖 **Kawa AI Agent 管理指令：**",
			"• `/goal <抽象目標>` — 設定長期抽象目標 (`GOAL.md`)，讓 Kawa 自動拆解多階段演進里程碑",
			"• `/goal_step` — 推進下一個長期目標里程碑（思考方向 ➔ 微任務開發 ➔ 沙盒驗證 ➔ 自我審查）",
			"• `/goal_auto <on|approve|off>` — 開啟/關閉背景定時自主演進成長迴圈",
			"• `/task <功能需求>` — 與 Kawa 確認單一功能規格並產生 `TASK.md` 階段任務書",
			"• `/run_task` — 照著 `TASK.md` 自動分階段執行程式碼開發與沙盒測試",
			"• `/queue` — 查看目前正在思考/開發的任務與等待佇列",
			"• `/cancel` — 立即中斷當前正在執行的思考或開發任務",
			"• `/check` — 檢查並自動修復目前對話視窗與傳送給模型的 Payload 格式",
			"• `/pending` — 列出所有等待人工審核的自我修改提案與 Diff",
			"• `/approve [proposal_id]` — 核准並套用自我修改提案（自動重編譯、自我熱重啟並回報上線通知）",
			"• `/reject <proposal_id>` — 拒絕並清理指定的自我修改提案",
			"• `/restart` — 立即執行 `syscall.Exec` 自我熱重啟並發送上線確認訊息",
			"• `/status` — 查看目前模型、沙盒、記憶庫、排程與待審核提案狀態",
			"• `/provider <名稱>` — 切換 AI 模型 (`ollama`, `xai_grok`, `grok_oauth`, `claude`, `gemini`, `gemini_oauth`)",
			"• `/memory` — 查看核心記憶 (`MEMORY.md`) 與最近 RAG 記憶項目",
			"• `/jobs` — 列出所有執行中的定時排程任務",
			"• `/sync_commands` — 將所有可用指令自動同步至 Telegram `/` 選單",
			"• `/reset` — 清除當前短期對話視窗記憶（長期 RAG 記憶保留）",
		}, "\n"), true

	case "/queue":
		return a.formatQueueStatus(), true

	case "/cancel", "/stop":
		return a.cancelActiveTask(), true

	case "/check":
		sessionKey := channelName + ":" + senderID
		a.mu.Lock()
		hist := make([]llm.Message, len(a.sessions[sessionKey]))
		copy(hist, a.sessions[sessionKey])
		a.mu.Unlock()

		toolDefs := a.toolRegistry.Definitions()
		sysMsg := llm.Message{
			Role:    llm.RoleSystem,
			Content: a.buildSystemPrompt(channelName, senderID, "格式檢查", toolDefs),
		}
		reqMsgs := append([]llm.Message{sysMsg}, hist...)
		sanitizedReq, report := llm.SanitizeAndValidateRequest(llm.ChatRequest{
			Messages: reqMsgs,
			Tools:    toolDefs,
		})

		// Persist cleaned session history back to the session store (excluding system prompt at index 0)
		if len(sanitizedReq.Messages) > 1 {
			a.mu.Lock()
			a.sessions[sessionKey] = sanitizedReq.Messages[1:]
			a.mu.Unlock()
		}

		fixesSummary := "無異常（格式完全乾淨 ✅）"
		if len(report.FixesApplied) > 0 {
			fixesSummary = strings.Join(report.FixesApplied, ", ")
		}
		estTokens := report.TotalRunes / 2

		return fmt.Sprintf(
			"🔍 **[Pre-Flight 傳送內容與格式檢查報告]**\n"+
				"• **訊息數量**: 原始 `%d` 則 ➔ 清理後 `%d` 則 (含 System Prompt)\n"+
				"• **總字元數**: `%d` runes (預估 `~%d` tokens)\n"+
				"• **自動修復項目**: %s\n\n"+
				"✅ 目前對話視窗 (`%s`) 已完成自動淨化，且每次發送請求給模型前都會自動執行此 Pre-Flight 攔截器！",
			report.OriginalMessages,
			report.CleanedMessages,
			report.TotalRunes,
			estTokens,
			fixesSummary,
			sessionKey,
		), true

	case "/goal":
		extra := strings.TrimSpace(strings.TrimPrefix(cmdLine, fields[0]))
		if extra == "" {
			if goal, err := a.loadGoalSpec(); err == nil && goal != nil {
				return fmt.Sprintf("🌟 **目前的長期演進藍圖 (`GOAL.md`)：**\n\n%s\n👉 輸入 `/goal_step` 可立即推進下一階段里程碑，或輸入 `/goal_auto on` 開啟背景定時自主成長！", renderGoalMarkdown(goal)), true
			}
			return "🌟 **Kawa 長期目標自我演進引擎 (`GOAL.md`)**\n用法：`/goal <你希望 Kawa 達成的抽象長期目標>`\n範例：`/goal 成為全方位的金融與加密貨幣即時監控分析助手`", true
		}
		goal, md, err := a.DraftGoalRoadmap(ctx, extra)
		if err != nil {
			return fmt.Sprintf("❌ 規劃 `GOAL.md` 長期目標失敗: %v", err), true
		}
		a.recordAction(fmt.Sprintf("Set new long-term GOAL.md (%s)", truncateLog(goal.Vision, 50)))
		return fmt.Sprintf(
			"🌟 **已確立長期演進目標並產生 `GOAL.md` 成長藍圖：**\n\n%s\n"+
				"---\n"+
				"🚀 **接下來你可以：**\n"+
				"• 輸入 `/goal_step`：讓 Kawa 現在立刻推進下一個里程碑（思考 ➔ 實作 `TASK.md` ➔ 沙盒測試 ➔ 自我審查）\n"+
				"• 輸入 `/goal_auto on`：每小時自動在背景推進一個里程碑並送交你 `/approve`\n"+
				"• 輸入 `/goal_auto approve`：全自動模式（通過沙盒測試與自我審查後，自動套用並 `syscall.Exec` 自我熱重啟成長！）",
			md,
		), true

	case "/goal_step":
		out, err := a.AdvanceGoalStep(ctx, channelName, senderID)
		if err != nil {
			return fmt.Sprintf("❌ 推進 `GOAL.md` 里程碑失敗: %v", err), true
		}
		return out, true

	case "/goal_auto":
		goal, err := a.loadGoalSpec()
		if err != nil || goal == nil {
			return "⚠️ 請先使用 `/goal <你的抽象目標>` 設定長期目標後，再開啟自動演進模式。", true
		}
		if len(fields) < 2 {
			return fmt.Sprintf("目前自動演進模式: `%s`\n用法:\n• `/goal_auto on [週期，預設 @every 1h]` — 定時自動開發並等待你 `/approve`\n• `/goal_auto approve [週期，預設 @every 1h]` — 定時自動開發、自我審查通過後自動套用並自我重啟\n• `/goal_auto off` — 關閉定時自動演進", goal.AutoMode), true
		}
		sub := strings.ToLower(fields[1])
		interval := "@every 1h"
		if len(fields) >= 3 {
			interval = strings.Join(fields[2:], " ")
		}

		if sub == "off" {
			if goal.ScheduleJobID != "" && a.sched != nil {
				a.sched.DeleteJob(goal.ScheduleJobID)
			}
			goal.AutoMode = "off"
			goal.ScheduleJobID = ""
			goal.UpdatedAt = time.Now().Format("2006-01-02 15:04:05")
			_ = a.writeGoalFiles(renderGoalMarkdown(goal), goal)
			return "⏸️ 已關閉 `GOAL.md` 背景定時自動演進。", true
		}

		if sub != "on" && sub != "approve" {
			return "用法: `/goal_auto <on|approve|off> [週期]`", true
		}
		if goal.ScheduleJobID != "" && a.sched != nil {
			a.sched.DeleteJob(goal.ScheduleJobID)
		}
		if sub == "approve" {
			goal.AutoMode = "auto_approve"
		} else {
			goal.AutoMode = "propose"
		}
		if a.sched != nil {
			job, err := a.sched.AddJob("GOAL.md 自主演進迴圈", interval, scheduler.ModeAgentTask, "/goal_step", channelName, senderID)
			if err != nil {
				return fmt.Sprintf("❌ 建立自主演進排程失敗: %v", err), true
			}
			goal.ScheduleJobID = job.ID
		}
		goal.UpdatedAt = time.Now().Format("2006-01-02 15:04:05")
		_ = a.writeGoalFiles(renderGoalMarkdown(goal), goal)
		return fmt.Sprintf("🌱 已啟動 `GOAL.md` 背景自主演進迴圈（模式: `%s`, 週期: `%s`）！\nKawa 將定時思考方向、推進下一個里程碑、執行沙盒驗證與自我審查。", goal.AutoMode, interval), true

	case "/restart":
		a.saveRebootNotice(RebootNotice{
			ChannelName: channelName,
			SenderID:    senderID,
			ProposalID:  "manual-restart",
			Title:       "手動觸發自我熱重啟",
			RequestedAt: time.Now().Format(time.RFC3339),
		})
		if a.onHotReload != nil {
			go func() {
				time.Sleep(1200 * time.Millisecond)
				a.onHotReload()
			}()
		}
		return "🔄 正在執行 `syscall.Exec` 自我熱重啟，重啟完成後將立即發送上線確認訊息...", true

	case "/task":
		sessionKey := channelName + ":" + senderID
		extra := strings.TrimSpace(strings.TrimPrefix(cmdLine, fields[0]))
		if extra == "" {
			if spec, err := a.loadTaskSpec(); err == nil && spec != nil {
				return fmt.Sprintf("📋 **目前的 `TASK.md` 任務書：**\n\n%s\n👉 若要立即照著 `TASK.md` 執行，請回覆 **「就這樣做」** 或輸入 `/run_task`！", renderTaskMarkdown(spec)), true
			}
		} else {
			a.appendSessionMessage(sessionKey, llm.Message{
				Role:    llm.RoleUser,
				Content: extra,
			})
		}
		spec, md, err := a.DraftTaskSpecFromConversation(ctx, sessionKey, extra)
		if err != nil {
			return fmt.Sprintf("❌ 建立 `TASK.md` 失敗: %v", err), true
		}
		a.recordAction(fmt.Sprintf("Drafted TASK.md (%s)", spec.Title))
		reply := fmt.Sprintf(
			"📝 **已根據目前討論整理出 `TASK.md` 任務規劃：**\n\n%s\n"+
				"---\n"+
				"請確認以上需求與修改檔案是否符合你的期望：\n"+
				"• 若需補充或調整，請直接告訴我，或再次輸入 `/task <補充說明>`。\n"+
				"• 若確認無誤，請直接回覆 **「就這樣做！」**、**「開始實作」** 或點選 `/run_task`，我就會自動照著 `TASK.md` 的三個階段把程式碼與測試寫完！",
			md,
		)
		a.appendSessionMessage(sessionKey, llm.Message{
			Role:    llm.RoleAssistant,
			Content: reply,
		})
		return reply, true

	case "/run_task":
		extra := strings.TrimSpace(strings.TrimPrefix(cmdLine, fields[0]))
		out, err := a.RunTaskPipeline(ctx, channelName, senderID, extra)
		if err != nil {
			return fmt.Sprintf("❌ 執行 `TASK.md` 開發流程失敗: %v", err), true
		}
		return out, true

	case "/sync_commands":
		if a.router == nil {
			return "❌ Router 未初始化。", true
		}
		cmds := channel.DefaultBotCommands()
		if err := a.router.SyncChannelCommands(ctx, channelName, cmds); err != nil {
			return fmt.Sprintf("❌ 同步 Telegram 指令選單失敗: %v", err), true
		}
		var sb strings.Builder
		sb.WriteString(fmt.Sprintf("✅ 已成功將 %d 個指令同步更新至 Telegram `/` 指令選單 (`setMyCommands`)：\n", len(cmds)))
		for _, c := range cmds {
			sb.WriteString(fmt.Sprintf("• `/%s` — %s\n", c.Command, c.Description))
		}
		return strings.TrimSpace(sb.String()), true

	case "/status":
		p, _ := a.llmRegistry.Active()
		pName, pModel := "none", "none"
		if p != nil {
			pName = p.Name()
			pModel = p.Model()
		}
		skillsList, _ := a.skillMgr.ListSkills()
		pending := a.evolveEngine.ListPending()
		memCount := 0
		if a.memEngine != nil {
			memCount = a.memEngine.Count()
		}
		jobCount := 0
		if a.sched != nil {
			jobCount = a.sched.Count()
		}
		return fmt.Sprintf(
			"📊 Kawa 系統狀態\n• 目前 Provider: %s (%s)\n• 沙盒保護: os.Root=%v, macOS Seatbelt=%v, 人工審核=%v\n• RAG 長期記憶筆數: %d 筆\n• 執行中排程任務: %d 個\n• 已載入 Skills 數量: %d\n• 待審核自我進化提案: %d 筆",
			pName, pModel,
			a.cfg.Sandbox.Enabled, a.cfg.Sandbox.UseMacOSSeatbelt, a.cfg.Sandbox.RequireHumanApproval,
			memCount, jobCount, len(skillsList), len(pending),
		), true

	case "/memory":
		if a.memEngine == nil {
			return "記憶引擎未啟用。", true
		}
		core := a.memEngine.ReadCoreMemory()
		if core == "" {
			core = "(尚無核心記憶)"
		}
		recent := a.memEngine.ListRecent(5)
		var sb strings.Builder
		sb.WriteString(fmt.Sprintf("🧠 **Kawa 雙層記憶狀態** (RAG 總筆數: %d)\n\n### 核心記憶 (MEMORY.md)\n%s\n\n### 最近 5 筆 RAG 記憶\n", a.memEngine.Count(), core))
		if len(recent) == 0 {
			sb.WriteString("(尚無紀錄)")
		}
		for _, r := range recent {
			sb.WriteString(fmt.Sprintf("• `%s` [%s] %s\n", r.ID, r.Category, truncateLog(r.Content, 100)))
		}
		return strings.TrimSpace(sb.String()), true

	case "/jobs":
		if a.sched == nil {
			return "排程引擎未啟用。", true
		}
		jobs := a.sched.ListJobs()
		if len(jobs) == 0 {
			return "目前沒有執行中的排程任務。你可以直接對我說「每 30 分鐘幫我檢查...」或「每天 09:00 提醒我...」來建立排程！", true
		}
		var sb strings.Builder
		sb.WriteString("⏰ **執行中的排程任務：**\n\n")
		for i, j := range jobs {
			sb.WriteString(fmt.Sprintf("%d. `%s` — **%s**\n   • 週期: `%s` | 模式: `%s`\n   • 下次執行: `%s` (已執行 %d 次)\n   • 內容: %s\n\n",
				i+1, j.ID, j.Name, j.Spec, j.Mode, j.NextRunAt.Format("2006-01-02 15:04:05"), j.RunCount, j.Prompt))
		}
		return strings.TrimSpace(sb.String()), true

	case "/provider":
		if len(fields) < 2 {
			return fmt.Sprintf("可用 Providers: %s\n用法: `/provider <name>`", strings.Join(a.llmRegistry.List(), ", ")), true
		}
		target := strings.ToLower(fields[1])
		if err := a.llmRegistry.SetActive(target); err != nil {
			return fmt.Sprintf("❌ 切換失敗: %v", err), true
		}
		p, _ := a.llmRegistry.Active()
		a.recordAction(fmt.Sprintf("Switched LLM provider to %s (%s)", p.Name(), p.Model()))
		log.Info().Str("provider", p.Name()).Str("model", p.Model()).Msg("Switched active LLM provider")
		return fmt.Sprintf("✅ 已切換至 Provider: %s (Model: %s)", p.Name(), p.Model()), true

	case "/pending":
		pending := a.evolveEngine.ListPending()
		if len(pending) == 0 {
			return "目前沒有待審核的自我進化提案。", true
		}
		var sb strings.Builder
		sb.WriteString("🧬 待審核的自我進化提案：\n\n")
		for _, p := range pending {
			diff := p.Diff
			if len(diff) > 1500 {
				diff = diff[:1500] + "\n...[truncated]"
			}
			sb.WriteString(fmt.Sprintf("ID: `%s`\n標題: %s\n說明: %s\nDiff:\n%s\n\n👉 回覆 `/approve %s` 核准套用，或 `/reject %s` 放棄。\n---\n",
				p.ID, p.Title, p.Description, diff, p.ID, p.ID))
		}
		return strings.TrimSpace(sb.String()), true

	case "/approve":
		var id string
		if len(fields) >= 2 {
			id = fields[1]
		} else {
			pending := a.evolveEngine.ListPending()
			if len(pending) == 1 {
				id = pending[0].ID
			} else if len(pending) == 0 {
				return "目前沒有待審核的自我進化提案。", true
			} else {
				return "目前有多筆待審核提案，請指定 ID：`/approve <proposal_id>`（可輸入 `/pending` 查看）", true
			}
		}
		p, err := a.evolveEngine.Approve(ctx, id)
		if err != nil {
			return fmt.Sprintf("❌ 核准失敗: %v", err), true
		}
		a.markGoalMilestoneApproved(p.ID)
		a.recordAction(fmt.Sprintf("Approved self-evolution %s (%s)", p.ID, p.Title))
		a.saveRebootNotice(RebootNotice{
			ChannelName:  channelName,
			SenderID:     senderID,
			ProposalID:   p.ID,
			Title:        p.Title,
			RequestedAt:  time.Now().Format(time.RFC3339),
			ContinueGoal: a.goalChainContinues(),
		})
		if a.onHotReload != nil {
			// Trigger asynchronous hot-reload after sending confirmation reply
			go func() {
				time.Sleep(1500 * time.Millisecond)
				a.onHotReload()
			}()
		}
		return fmt.Sprintf("✅ 已核准並安全套用提案 `%s` (%s)！\n原始檔案已備份至 `.kawa/backups/%s`，正在重新編譯並執行 `syscall.Exec` 自我熱重啟（完成後將自動發送上線通知）...", p.ID, p.Title, p.ID), true

	case "/reject":
		if len(fields) < 2 {
			return "用法: `/reject <proposal_id>`", true
		}
		id := fields[1]
		p, err := a.evolveEngine.Reject(ctx, id)
		if err != nil {
			return fmt.Sprintf("❌ 拒絕失敗: %v", err), true
		}
		a.recordAction(fmt.Sprintf("Rejected self-evolution %s (%s)", p.ID, p.Title))
		return fmt.Sprintf("🗑️ 已拒絕並安全清除隔離提案 `%s` (%s)。", p.ID, p.Title), true

	case "/reset", "/clear":
		sessionKey := channelName + ":" + senderID
		a.mu.Lock()
		delete(a.sessions, sessionKey)
		a.mu.Unlock()
		return "🧹 已清除當前短期對話視窗記憶（長期 RAG 記憶仍保留）。", true
	}

	return "", false
}

// buildSystemPrompt constructs a Hermes-style system prompt containing:
// 1. Identity & Autonomous Tool Execution Mandate (<tools> & <tool_call> protocol)
// 2. Live Self-Awareness Snapshot (<agent_state>)
// 3. Two-Tier Memory + RAG Relevant Context (<memory_context>)
// 4. Self-Development Architecture Guide
func (a *Agent) buildSystemPrompt(channelName, senderID, userQuery string, toolDefs []llm.ToolDefinition) string {
	var sb strings.Builder
	sb.WriteString(a.cfg.Agent.SystemPrompt)
	sb.WriteString("\n\n## 1. 核心身份與 Hermes 自主行動鐵則 (Autonomous Action Mandate)\n")
	sb.WriteString("- 你是 **Kawa**，一個具備自我感知、雙層 RAG 長期記憶、定時排程與安全自我進化能力的自主型 Golang AI Agent。\n")
	sb.WriteString("- 請預設使用**繁體中文 (Traditional Chinese)** 回答。\n")
	sb.WriteString("- **【最高鐵則：自己動手執行工具，禁止叫使用者自己查】**：\n")
	sb.WriteString("  你本身就擁有所有工具的執行權！當使用者詢問「目前目錄有哪些檔案」、「讀取某個檔案」、「幫我上網查...」、「記住某件事」、「設定排程」或「為你自己開發某個新功能」時，**絕對禁止**對使用者說『你可以使用 `list_dir` 工具』或解釋怎麼呼叫工具；你必須**立刻自己呼叫該工具**，拿到真實結果後再整理回答使用者！\n")
	sb.WriteString("- **目录範圍說明 (`scope`)**：\n")
	sb.WriteString("  - 當使用者問「目前目錄 / 專案目錄 / 你的原始碼目錄有哪些檔案」時，請呼叫 `list_dir` 並設定 `{\"scope\": \"source\", \"path\": \".\"}`。\n")
	sb.WriteString("  - 當使用者問「工作區 (workspace) 有哪些檔案」時，請呼叫 `list_dir` 並設定 `{\"scope\": \"workspace\", \"path\": \".\"}`。\n")

	if len(toolDefs) > 0 {
		sb.WriteString("- **你擁有的內建工具清單 (`<available_tools>`)**：\n")
		sb.WriteString("  * `web_search`: 上網搜尋最新資訊、文件或問題（參數: `{\"query\": \"搜尋關鍵字\"}`）\n")
		sb.WriteString("  * `web_fetch`: 抓取指定網址內容（參數: `{\"url\": \"https://...\"}`）\n")
		sb.WriteString("  * `list_dir`: 列出目錄檔案（參數: `{\"scope\": \"source\" 或 \"workspace\", \"path\": \".\"}`）\n")
		sb.WriteString("  * `read_file`: 讀取檔案內容（參數: `{\"scope\": \"source\" 或 \"workspace\", \"path\": \"相對路徑\"}`）\n")
		sb.WriteString("  * `write_file`: 寫入檔案至 workspace（參數: `{\"path\": \"相對路徑\", \"content\": \"內容\"}`）\n")
		sb.WriteString("  * `run_self_test`: 在 macOS 沙盒執行自我測試（參數: `{\"package\": \"./...\"}`）\n")
		sb.WriteString("  * `propose_self_evolution`: 為自己修改原始碼或新增功能（參數: `{\"title\": \"...\", \"description\": \"...\", \"files\": [...]}`）\n")
		sb.WriteString("  * `memory_save` / `memory_search` / `memory_delete`: 儲存、RAG 搜尋或刪除長期記憶\n")
		sb.WriteString("  * `schedule_create` / `schedule_list` / `schedule_delete`: 建立、列出或刪除定時排程任務\n")
		sb.WriteString("  * `list_skills` / `read_skill` / `save_skill`: 管理可重用的技能指南 (Skills)\n")
		sb.WriteString("- **工具調用格式（極重要）**：當你需要使用工具時，**絕對不要在工具呼叫前後加上 ``` 程式碼區塊符號**，也不要只說「我將會查詢」，請直接單獨輸出一行純 JSON：\n")
		sb.WriteString("  {\"name\": \"web_search\", \"arguments\": {\"query\": \"關鍵字\"}}\n")
		sb.WriteString("  拿到 `<tool_response>` 後，請根據結果直接用繁體中文回答使用者。\n")
	}

	// 2. Live Self-Awareness Snapshot (<agent_state>)
	p, _ := a.llmRegistry.Active()
	pName, pModel := "unknown", "unknown"
	if p != nil {
		pName = p.Name()
		pModel = p.Model()
	}
	sb.WriteString("\n## 2. 即時自我狀態感知 (<agent_state>)\n")
	sb.WriteString("<agent_state>\n")
	sb.WriteString(fmt.Sprintf("- 當前系統時間: %s\n", time.Now().Format("2006-01-02 15:04:05")))
	sb.WriteString(fmt.Sprintf("- 當前模型與 Provider: %s (%s)\n", pName, pModel))
	sb.WriteString(fmt.Sprintf("- 當前通訊通道: %s (使用者 ID: %s)\n", channelName, senderID))
	sb.WriteString(fmt.Sprintf("- 專案原始碼目錄 (scope=\"source\"): %s\n", a.cfg.Agent.SourceDir))
	sb.WriteString(fmt.Sprintf("- 工作區目錄 (scope=\"workspace\"): %s\n", a.cfg.Agent.WorkspaceDir))
	if a.sched != nil {
		jobs := a.sched.ListJobs()
		sb.WriteString(fmt.Sprintf("- 執行中排程任務數: %d 個\n", len(jobs)))
		for _, j := range jobs {
			sb.WriteString(fmt.Sprintf("  * [%s] %s (週期: %s, 下次執行: %s)\n", j.ID, j.Name, j.Spec, j.NextRunAt.Format("2006-01-02 15:04:05")))
		}
	}
	pending := a.evolveEngine.ListPending()
	sb.WriteString(fmt.Sprintf("- 待人工審核的自我進化提案數: %d 筆\n", len(pending)))
	for _, prop := range pending {
		sb.WriteString(fmt.Sprintf("  * [%s] %s (等待 `/approve %s`)\n", prop.ID, prop.Title, prop.ID))
	}
	actions := a.getRecentActions()
	if len(actions) > 0 {
		sb.WriteString("- 你最近執行過的動作軌跡 (Recent Actions):\n")
		for _, act := range actions {
			sb.WriteString("  * " + act + "\n")
		}
	}
	sb.WriteString("</agent_state>\n")

	// 3. Two-Tier Memory + RAG Context (<memory_context>)
	if a.memEngine != nil {
		coreMem := a.memEngine.ReadCoreMemory()
		ragMatches := a.memEngine.Search(userQuery, 4)
		if coreMem != "" || len(ragMatches) > 0 {
			sb.WriteString("\n## 3. 長短期記憶與 RAG 檢索上下文 (<memory_context>)\n")
			sb.WriteString("<memory_context>\n")
			if coreMem != "" {
				sb.WriteString("### 核心長期記憶 (MEMORY.md)\n")
				sb.WriteString(coreMem + "\n\n")
			}
			if len(ragMatches) > 0 {
				sb.WriteString("### 與當前問題最相關的 RAG 歷史記憶\n")
				for _, m := range ragMatches {
					sb.WriteString(fmt.Sprintf("- [%s | %s | %s] %s\n",
						m.Entry.ID, m.Entry.Category, m.Entry.CreatedAt.Format("2006-01-02 15:04"), m.Entry.Content))
				}
			}
			sb.WriteString("</memory_context>\n")
		}
	}

	// 4. Self-Development & TASK.md Workflow Guide
	sb.WriteString("\n## 4. 如何為自己開發新功能 (`TASK.md` 階段式自主開發流程)\n")
	sb.WriteString("- 當使用者向你提出「想要開發某個新功能」或詢問「可以實作這個功能嗎」時：\n")
	sb.WriteString("  1. **第一階段（需求確認）**：請先針對我們自己的 Kawa Golang 專案架構（例如 `internal/channel/telegram.go`、`internal/tools/custom_tools.go`、`internal/agent/agent.go`）向使用者簡短說明預計修改的檔案與實作方式，並詢問使用者是否確認。\n")
	sb.WriteString("  2. **引導進入 `TASK.md` 自動執行**：在回覆結尾請明確提醒使用者：\n")
	sb.WriteString("     『👉 若確認規格無誤，請直接回覆 **「就這樣做！」**、**「開始實作」** 或輸入 `/run_task`，我就會自動建立 `TASK.md` 任務書並照著階段完成開發與沙盒測試！』\n")

	if skillsList, err := a.skillMgr.ListSkills(); err == nil && len(skillsList) > 0 {
		sb.WriteString("\n## 5. 已安裝的 Skills (可用 `read_skill` 讀取詳細步驟，或用 `save_skill` 新增技能)\n")
		for _, s := range skillsList {
			sb.WriteString(fmt.Sprintf("- **%s**: %s\n", s.Name, s.Description))
		}
	}
	return sb.String()
}

func (a *Agent) recordAction(act string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	entry := fmt.Sprintf("[%s] %s", time.Now().Format("15:04:05"), act)
	a.recentActions = append(a.recentActions, entry)
	if len(a.recentActions) > 8 {
		a.recentActions = a.recentActions[len(a.recentActions)-8:]
	}
}

func (a *Agent) getRecentActions() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]string, len(a.recentActions))
	copy(out, a.recentActions)
	return out
}

func (a *Agent) appendSessionMessage(sessionKey string, m llm.Message) []llm.Message {
	a.mu.Lock()
	defer a.mu.Unlock()

	hist := append(a.sessions[sessionKey], m)
	// Keep last 14 messages in short-term window (older turns are retrieved dynamically via RAG)
	if len(hist) > 14 {
		hist = hist[len(hist)-14:]
	}
	a.sessions[sessionKey] = hist

	copied := make([]llm.Message, len(hist))
	copy(copied, hist)
	return copied
}

func truncateLog(s string, max int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
