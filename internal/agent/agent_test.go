package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fanice1998/kawa/internal/channel"
	"github.com/fanice1998/kawa/internal/config"
	"github.com/fanice1998/kawa/internal/evolution"
	"github.com/fanice1998/kawa/internal/llm"
	"github.com/fanice1998/kawa/internal/memory"
	"github.com/fanice1998/kawa/internal/sandbox"
	"github.com/fanice1998/kawa/internal/scheduler"
	"github.com/fanice1998/kawa/internal/skills"
	"github.com/fanice1998/kawa/internal/tools"
)

func TestAgentAutonomousToolsMemoryAndSelfAwareness(t *testing.T) {
	var callStep int32

	// Simulate an Ollama qwen2.5-coder server
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodyStr := string(body)
		step := atomic.AddInt32(&callStep, 1)

		w.Header().Set("Content-Type", "application/json")
		switch step {
		case 1:
			// Step 1: Simulate lazy model telling user "你可以使用 list_dir" instead of calling it!
			_, _ = w.Write([]byte(`{"message":{"role":"assistant","content":"你可以使用 list_dir 工具來查看目前目錄有哪些檔案。"}}`))
		case 2:
			// Step 2: Agent's Auto-Correction Nudge kicked in! Now model outputs Hermes <tool_call>
			if !strings.Contains(bodyStr, "System Autonomous Correction") {
				t.Errorf("expected System Autonomous Correction nudge in step 2 request")
			}
			_, _ = w.Write([]byte(`{"message":{"role":"assistant","content":"<tool_call>\n{\"name\": \"list_dir\", \"arguments\": {\"scope\": \"source\", \"path\": \".\"}}\n</tool_call>"}}`))
		case 3:
			// Step 3: Model receives <tool_response> with real directory listing (go.mod, README.md, internal/)
			if !strings.Contains(bodyStr, "go.mod") || !strings.Contains(bodyStr, "README.md") {
				t.Errorf("expected real list_dir output (go.mod, README.md) in step 3 request, got: %s", bodyStr)
			}
			_, _ = w.Write([]byte(`{"message":{"role":"assistant","content":"目前專案目錄包含：go.mod, README.md, cmd/, internal/, skills/。"}}`))
		case 4:
			// Step 4: User asks to remember name Fanice -> model emits inline JSON tool call
			_, _ = w.Write([]byte(`{"message":{"role":"assistant","content":"{\"name\": \"memory_save\", \"arguments\": {\"category\": \"preference\", \"content\": \"使用者的名字是 Fanice，最喜歡 Golang\"}}"}}`))
		case 5:
			// Step 5: Memory saved confirmation
			_, _ = w.Write([]byte(`{"message":{"role":"assistant","content":"好的，我已經記住你的名字是 Fanice，且最喜歡 Golang！"}}`))
		case 6:
			// Step 6: After /reset, verify <agent_state> and <memory_context> (with Fanice) are injected into System Prompt!
			var reqPayload struct {
				Messages []struct {
					Role    string `json:"role"`
					Content string `json:"content"`
				} `json:"messages"`
			}
			_ = json.Unmarshal(body, &reqPayload)
			sysPrompt := ""
			if len(reqPayload.Messages) > 0 {
				sysPrompt = reqPayload.Messages[0].Content
			}
			if !strings.Contains(sysPrompt, "<agent_state>") || !strings.Contains(sysPrompt, "Executed tool list_dir") {
				t.Errorf("expected <agent_state> with recent action history in system prompt, got:\n%s", sysPrompt)
			}
			if !strings.Contains(sysPrompt, "<memory_context>") || !strings.Contains(sysPrompt, "Fanice") {
				t.Errorf("expected <memory_context> with RAG/Core memory 'Fanice' in system prompt after /reset, got:\n%s", sysPrompt)
			}
			_, _ = w.Write([]byte(`{"message":{"role":"assistant","content":"我目前正在 Telegram 通道為你服務，剛剛幫你執行過 list_dir 與 memory_save。當然記得，你是 Fanice，最喜歡 Golang！"}}`))
		}
	}))
	defer srv.Close()

	tmpState := t.TempDir()
	srcDir, _ := filepath.Abs(filepath.Join("..", ".."))
	cfg := &config.Config{
		Agent: config.AgentConfig{
			Name:            "Kawa",
			WorkspaceDir:    filepath.Join(tmpState, "workspace"),
			SourceDir:       srcDir,
			SkillsDir:       filepath.Join(srcDir, "skills"),
			MaxIterations:   10,
			DefaultProvider: "ollama",
		},
		Sandbox: config.SandboxConfig{
			Enabled:              true,
			UseMacOSSeatbelt:     true,
			RequireHumanApproval: true,
			StateDir:             tmpState,
		},
		LLM: config.LLMConfig{
			Ollama: config.OllamaConfig{
				BaseURL: srv.URL,
				Model:   "qwen2.5-coder:14b",
			},
		},
	}

	wsJail, _ := sandbox.NewFSJail(cfg.Agent.WorkspaceDir, "", false)
	srcJail, _ := sandbox.NewFSJail(cfg.Agent.SourceDir, "", true)
	runner, _ := sandbox.NewSeatbeltRunner(true, tmpState, 30*time.Second)
	memEngine, _ := memory.NewEngine(tmpState)
	sched, _ := scheduler.NewScheduler(tmpState, nil)
	skillMgr, _ := skills.NewManager(cfg.Agent.SkillsDir, "")
	evolveEngine, _ := evolution.NewEngine(cfg.Agent.SourceDir, tmpState, runner, nil)

	toolReg := tools.NewRegistry()
	toolReg.Register(tools.NewReadFileTool(wsJail, srcJail))
	toolReg.Register(tools.NewWriteFileTool(wsJail))
	toolReg.Register(tools.NewListDirTool(wsJail, srcJail))
	toolReg.Register(tools.NewMemorySaveTool(memEngine))
	toolReg.Register(tools.NewMemorySearchTool(memEngine))
	toolReg.Register(tools.NewScheduleCreateTool(sched))
	toolReg.Register(tools.NewScheduleListTool(sched))

	llmReg := llm.NewRegistry(cfg.LLM, "ollama")
	router := channel.NewRouter()

	ag := New(cfg, llmReg, toolReg, skillMgr, memEngine, sched, evolveEngine, router, nil)
	ctx := context.Background()

	// 1. Test lazy tool mention interception -> automatic list_dir execution
	r1, err := ag.ProcessMessage(ctx, "telegram", "795566183", "目前目錄有哪些檔案？")
	if err != nil {
		t.Fatalf("Q1 failed: %v", err)
	}
	if !strings.Contains(r1, "go.mod") {
		t.Fatalf("expected final answer to list go.mod after autonomous tool call, got: %s", r1)
	}

	// 2. Test memory_save tool call
	r2, err := ag.ProcessMessage(ctx, "telegram", "795566183", "幫我記住我是 Fanice，最喜歡 Golang")
	if err != nil {
		t.Fatalf("Q2 failed: %v", err)
	}
	if !strings.Contains(r2, "Fanice") {
		t.Fatalf("expected confirmation mentioning Fanice, got: %s", r2)
	}

	// 3. Clear short-term session and verify <agent_state> + <memory_context> RAG recall
	_, _ = ag.ProcessMessage(ctx, "telegram", "795566183", "/reset")
	r3, err := ag.ProcessMessage(ctx, "telegram", "795566183", "你現在在幹嘛？還記得我的名字嗎？")
	if err != nil {
		t.Fatalf("Q3 failed: %v", err)
	}
	if !strings.Contains(r3, "Fanice") {
		t.Fatalf("expected recall of Fanice after /reset, got: %s", r3)
	}
}
