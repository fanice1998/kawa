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
	}
	a.recordAction("Agent initialized and ready")
	return a
}

// Run processes incoming messages and starts the autonomous scheduler until ctx is cancelled.
func (a *Agent) Run(ctx context.Context) error {
	a.router.StartAll(ctx)
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
			go a.handleInbound(ctx, msg)
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

	taskPrompt := fmt.Sprintf("[系統排程自動喚醒任務: %s]\n請立即自主呼叫所需工具完成以下排程任務，並將結果整理回報：\n%s", job.Name, job.Prompt)
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
		Str("text", truncateLog(msg.Text, 120)).
		Msg("Received inbound message")

	a.recordAction(fmt.Sprintf("Received message from %s:%s (%s)", msg.ChannelName, msg.SenderID, truncateLog(msg.Text, 40)))

	reply, err := a.ProcessMessage(ctx, msg.ChannelName, msg.SenderID, msg.Text)
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
	nudgedForLazyTool := false

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
		if len(resp.ToolCalls) == 0 && len(toolDefs) > 0 && !nudgedForLazyTool {
			if mentionedTool := mentionsToolWithoutCalling(resp.Content, toolDefs); mentionedTool != "" {
				nudgedForLazyTool = true
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
				nudgedForLazyTool = true
				log.Warn().
					Str("content", truncateLog(resp.Content, 100)).
					Msg("Intercepted incomplete or false-refusal response; forcing clean JSON tool call")

				history = a.appendSessionMessage(sessionKey, llm.Message{
					Role:    llm.RoleAssistant,
					Content: resp.Content,
				})
				nudge := fmt.Sprintf(
					"[System Autonomous Correction] 注意：你擁有完整的上網與檔案工具（包含 `web_search`、`web_fetch`、`list_dir`、`read_file` 等）！禁止回答「無法執行網路查詢」或只說「我將會查詢」。請針對使用者的問題 (%q)，現在立刻只輸出一行純 JSON 呼叫對應工具，例如：\n{\"name\": \"web_search\", \"arguments\": {\"query\": \"grok ai tools\"}}",
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

// isIncompleteToolAttempt detects when a model says "好的，我將會查詢..." or ends with dangling ```
// because Ollama's parser stripped or truncated the <tool_call> block.
func isIncompleteToolAttempt(content string) bool {
	trimmed := strings.TrimSpace(content)
	if strings.HasSuffix(trimmed, "```") || strings.HasSuffix(trimmed, "```json") || strings.HasSuffix(trimmed, "```xml") || strings.Contains(trimmed, "<tool_call") {
		return true
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

	switch cmd {
	case "/help":
		return strings.Join([]string{
			"🤖 Kawa AI Agent 管理指令：",
			"• `/status` — 查看目前模型、沙盒、記憶庫、排程與待審核提案狀態",
			"• `/provider <名稱>` — 切換 AI 模型 (`ollama`, `xai_grok`, `grok_oauth`, `claude`, `gemini`, `gemini_oauth`)",
			"• `/memory` — 查看核心記憶 (`MEMORY.md`) 與最近 RAG 記憶項目",
			"• `/jobs` — 列出所有執行中的定時排程任務",
			"• `/pending` — 列出所有等待人工審核的自我修改提案與 Diff",
			"• `/approve <proposal_id>` — 核准並套用自我修改提案（自動重編譯並熱重啟）",
			"• `/reject <proposal_id>` — 拒絕並清理指定的自我修改提案",
			"• `/reset` — 清除當前短期對話視窗記憶（長期 RAG 記憶保留）",
		}, "\n"), true

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
		if len(fields) < 2 {
			return "用法: `/approve <proposal_id>`", true
		}
		id := fields[1]
		p, err := a.evolveEngine.Approve(ctx, id)
		if err != nil {
			return fmt.Sprintf("❌ 核准失敗: %v", err), true
		}
		a.recordAction(fmt.Sprintf("Approved self-evolution %s (%s)", p.ID, p.Title))
		if a.onHotReload != nil {
			// Trigger asynchronous hot-reload after sending confirmation reply
			go func() {
				time.Sleep(1500 * time.Millisecond)
				a.onHotReload()
			}()
		}
		return fmt.Sprintf("✅ 已核准並安全套用提案 `%s` (%s)！\n原始檔案已備份至 `.kawa/backups/%s`，正在重新編譯並熱重啟 Kawa 使新功能立即生效...", p.ID, p.Title, p.ID), true

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

	// 4. Self-Development Architecture Guide
	sb.WriteString("\n## 4. 如何為自己開發新功能 (Self-Development Architecture)\n")
	sb.WriteString("- 當使用者要求你為自己新增工具或開發新功能時：\n")
	sb.WriteString("  1. 先呼叫 `read_file` (`scope: \"source\"`, `path: \"internal/tools/custom_tools.go\"`) 與 `internal/tools/tool.go` 了解介面。\n")
	sb.WriteString("  2. 呼叫 `propose_self_evolution` 提交新檔案（例如 `internal/tools/<name>_tool.go`、`internal/tools/<name>_tool_test.go`）以及更新後的 `internal/tools/custom_tools.go`（在 `RegisterCustomTools` 中呼叫 `reg.Register(...)`）。\n")
	sb.WriteString("  3. `propose_self_evolution` 會自動在隔離 Git Worktree 與 macOS `sandbox-exec` 沙盒中執行 `go vet`、`go test` 與 `go build`。通過後請把提案 ID 告訴使用者，使用者回覆 `/approve <id>` 後系統就會自動合併、重編譯並熱重啟！\n")

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
