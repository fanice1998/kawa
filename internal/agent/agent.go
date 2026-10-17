package agent

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/fanice1998/kawa/internal/channel"
	"github.com/fanice1998/kawa/internal/config"
	"github.com/fanice1998/kawa/internal/evolution"
	"github.com/fanice1998/kawa/internal/llm"
	"github.com/fanice1998/kawa/internal/skills"
	"github.com/fanice1998/kawa/internal/tools"
	"github.com/rs/zerolog/log"
)

// Agent coordinates LLM providers, sandboxed tools, skills, self-evolution, and communication channels.
type Agent struct {
	cfg          *config.Config
	llmRegistry  *llm.Registry
	toolRegistry *tools.Registry
	skillMgr     *skills.Manager
	evolveEngine *evolution.Engine
	router       *channel.Router

	mu       sync.Mutex
	sessions map[string][]llm.Message
}

func New(
	cfg *config.Config,
	llmRegistry *llm.Registry,
	toolRegistry *tools.Registry,
	skillMgr *skills.Manager,
	evolveEngine *evolution.Engine,
	router *channel.Router,
) *Agent {
	return &Agent{
		cfg:          cfg,
		llmRegistry:  llmRegistry,
		toolRegistry: toolRegistry,
		skillMgr:     skillMgr,
		evolveEngine: evolveEngine,
		router:       router,
		sessions:     make(map[string][]llm.Message),
	}
}

// Run processes incoming messages from all registered channels until ctx is cancelled.
func (a *Agent) Run(ctx context.Context) error {
	a.router.StartAll(ctx)
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

func (a *Agent) handleInbound(ctx context.Context, msg channel.InboundMessage) {
	log.Info().
		Str("channel", msg.ChannelName).
		Str("sender", msg.SenderID).
		Str("text", truncateLog(msg.Text, 120)).
		Msg("Received inbound message")

	reply, err := a.ProcessMessage(ctx, msg.ChannelName, msg.SenderID, msg.Text)
	if err != nil {
		log.Error().Err(err).Str("channel", msg.ChannelName).Msg("Failed to process message")
		reply = fmt.Sprintf("⚠️ 執行發生錯誤: %v", err)
	}

	if err := a.router.Send(ctx, msg.ChannelName, msg.SenderID, reply); err != nil {
		log.Error().Err(err).Str("channel", msg.ChannelName).Str("sender", msg.SenderID).Msg("Failed to send reply")
	}
}

// ProcessMessage handles slash commands or executes the multi-step ReAct tool-calling loop.
func (a *Agent) ProcessMessage(ctx context.Context, channelName, senderID, text string) (string, error) {
	trimmed := strings.TrimSpace(text)
	if strings.HasPrefix(trimmed, "/") {
		if cmdReply, handled := a.handleCommand(ctx, channelName, senderID, trimmed); handled {
			return cmdReply, nil
		}
	}

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
		// Do not attach tools for simple greetings ("你好", "你在幹嘛", "hi", etc.)
		// so coding models never hallucinate random tool calls on casual chat.
		toolDefs = nil
	}

	sysMsg := llm.Message{
		Role:    llm.RoleSystem,
		Content: a.buildSystemPrompt(),
	}

	executedCalls := make(map[string]bool)

	for iter := 1; iter <= maxIter; iter++ {
		reqMsgs := make([]llm.Message, 0, len(history)+1)
		reqMsgs = append(reqMsgs, sysMsg)
		reqMsgs = append(reqMsgs, history...)

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

		// Check if the model is trying to repeat the exact same tool call it already ran in this turn
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
			// Model tried to loop on an already-executed tool call; ask once without tools for a final text answer
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
			if strings.TrimSpace(resp.Content) == "" {
				return "(模型未回傳文字內容)", nil
			}
			return resp.Content, nil
		}

		for _, tc := range resp.ToolCalls {
			log.Info().
				Str("tool", tc.Name).
				Str("call_id", tc.ID).
				Str("args", truncateLog(tc.Arguments, 160)).
				Msg("Executing sandboxed tool")

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

// isCasualGreeting returns true for short greetings or chitchat that clearly do not need file/web/code tools.
func isCasualGreeting(text string) bool {
	cleaned := strings.ToLower(strings.TrimSpace(text))
	cleaned = strings.Trim(cleaned, "!！?？.。~～,， ")
	casualPhrases := map[string]bool{
		"hi": true, "hello": true, "hey": true, "yo": true, "test": true,
		"你好": true, "你好呀": true, "你好啊": true, "哈囉": true, "嗨": true,
		"早安": true, "午安": true, "晚安": true, "安安": true,
		"你在幹嘛": true, "在幹嘛": true, "你在做什麼": true, "你是誰": true,
		"介紹一下你自己": true, "謝謝": true, "感謝": true, "ok": true, "好": true,
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
			"• `/status` — 查看目前模型、沙盒狀態、已安裝 Skills 與待審核提案",
			"• `/provider <名稱>` — 切換 AI 模型 (`ollama`, `xai_grok`, `grok_oauth`, `claude`, `gemini`, `gemini_oauth`)",
			"• `/pending` — 列出所有等待人工審核的自我修改提案與 Diff",
			"• `/approve <proposal_id>` — 核准並套用通過沙盒測試的自我修改提案",
			"• `/reject <proposal_id>` — 拒絕並清理指定的自我修改提案",
			"• `/reset` — 清除當前對話歷史記憶",
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
		return fmt.Sprintf(
			"📊 Kawa 系統狀態\n• 目前 Provider: %s (%s)\n• 沙盒保護: os.Root=%v, macOS Seatbelt=%v, 人工審核=%v\n• 已載入 Skills 數量: %d\n• 待審核自我進化提案: %d 筆",
			pName, pModel,
			a.cfg.Sandbox.Enabled, a.cfg.Sandbox.UseMacOSSeatbelt, a.cfg.Sandbox.RequireHumanApproval,
			len(skillsList), len(pending),
		), true

	case "/provider":
		if len(fields) < 2 {
			return fmt.Sprintf("可用 Providers: %s\n用法: `/provider <name>`", strings.Join(a.llmRegistry.List(), ", ")), true
		}
		target := strings.ToLower(fields[1])
		if err := a.llmRegistry.SetActive(target); err != nil {
			return fmt.Sprintf("❌ 切換失敗: %v", err), true
		}
		p, _ := a.llmRegistry.Active()
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
		return fmt.Sprintf("✅ 已核准並安全套用提案 `%s` (%s)！原始檔案已自動備份至 `.kawa/backups/%s`。", p.ID, p.Title, p.ID), true

	case "/reject":
		if len(fields) < 2 {
			return "用法: `/reject <proposal_id>`", true
		}
		id := fields[1]
		p, err := a.evolveEngine.Reject(ctx, id)
		if err != nil {
			return fmt.Sprintf("❌ 拒絕失敗: %v", err), true
		}
		return fmt.Sprintf("🗑️ 已拒絕並安全清除隔離提案 `%s` (%s)。", p.ID, p.Title), true

	case "/reset", "/clear":
		sessionKey := channelName + ":" + senderID
		a.mu.Lock()
		delete(a.sessions, sessionKey)
		a.mu.Unlock()
		return "🧹 已清除當前對話記憶。", true
	}

	return "", false
}

func (a *Agent) buildSystemPrompt() string {
	var sb strings.Builder
	sb.WriteString(a.cfg.Agent.SystemPrompt)
	sb.WriteString("\n\n## 核心對話與工具使用原則 (Core Rules)\n")
	sb.WriteString("1. 請預設使用**繁體中文 (Traditional Chinese)** 自然、親切地與使用者溝通。\n")
	sb.WriteString("2. **何時不該使用工具**：當使用者只是打招呼（如「你好」、「你在幹嘛」）、閒聊、或詢問不需查檔/上網的一般問題時，請直接用純文字回答，**絕對不要呼叫任何工具，也不要輸出 JSON**。\n")
	sb.WriteString("3. **何時使用工具**：只有在使用者明確要求上網查資料 (`web_search`/`web_fetch`)、讀寫檔案 (`read_file`/`write_file`/`list_dir`)、執行測試 (`run_self_test`)、管理技能 (`save_skill`/`read_skill`/`list_skills`) 或自我修改進化 (`propose_self_evolution`) 時才呼叫工具。\n")
	sb.WriteString("4. **工具結果總結**：當工具執行完畢並回傳結果後，請根據結果直接用繁體中文向使用者總結說明，不要再輸出 JSON。\n")
	sb.WriteString("5. **三層安全沙盒**：\n")
	sb.WriteString("   - 讀取自身 Go 原始碼請用 `list_dir` / `read_file` 並指定 `scope: \"source\"`。\n")
	sb.WriteString("   - 修改自身 Go 原始碼或新增功能請一律呼叫 `propose_self_evolution`（會在獨立 Git Worktree 與 macOS `sandbox-exec` 沙盒中執行 `go vet`、`go test`、`go build`，並將 Diff 交由使用者 `/approve <id>` 審核）。\n")

	if skillsList, err := a.skillMgr.ListSkills(); err == nil && len(skillsList) > 0 {
		sb.WriteString("\n## 已安裝的 Skills (需要時可用 `read_skill` 查看詳細步驟)\n")
		for _, s := range skillsList {
			sb.WriteString(fmt.Sprintf("- **%s**: %s\n", s.Name, s.Description))
		}
	}
	return sb.String()
}

func (a *Agent) appendSessionMessage(sessionKey string, m llm.Message) []llm.Message {
	a.mu.Lock()
	defer a.mu.Unlock()

	hist := append(a.sessions[sessionKey], m)
	// Keep last 40 messages per session to bound context window
	if len(hist) > 40 {
		hist = hist[len(hist)-40:]
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
