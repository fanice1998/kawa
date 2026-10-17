# Kawa — 安全自我進化 Golang AI Agent

Kawa 是一個以 Go (1.24+ / 1.27) 打造、具備**自我修改編碼、自我測試、新增工具與技能 (Skills)、自我優化**能力的 AI Agent 框架，專為 macOS 環境設計了**三層核心級安全沙盒**，徹底杜絕 AI 誤刪或破壞宿主機系統檔案的風險。

## 🛡️ 三層安全沙盒架構

1. **零任意指令執行 (No Arbitrary Shell/Delete Tool)**：
   - 徹底廢除通用 `bash`/`sh`/`rm` 工具，Agent 僅能呼叫強型別的 Go Tool。
2. **Layer 1 — Go `os.Root` 檔案系統監獄 (`internal/sandbox/fs_jail.go`)**：
   - 使用 Go 原生 `os.OpenRoot` API，從作業系統 File Descriptor 層級鎖死讀寫範圍，徹底阻擋 `../` 路徑穿越與 Symlink 逃逸。
   - 正常對話時，Kawa 自身的原始碼目錄 (`source`) 為**嚴格唯讀**；所有覆寫操作皆自動在 `.kawa/backups/` 產生時間戳備份。
3. **Layer 2 — macOS `sandbox-exec` (Seatbelt) 核心級沙盒 (`internal/sandbox/seatbelt.go`)**：
   - 執行 `go vet`、`go test`、`go build` 時，強制套用動態產生的 Seatbelt 規則 `(deny file-write*)`，**核心級禁止對隔離區以外的任何電腦目錄進行寫入或刪除**。
4. **Layer 3 — Git Worktree 隔離進化 + 通訊軟體人工審核 (`internal/evolution/engine.go`)**：
   - 當要求 Kawa 修改自身程式碼或新增功能時，修改會在獨立的 `.kawa/worktrees/evolve-<id>` 中進行並通過沙盒測試，接著產生 Git Diff 發送至 Telegram / iMessage，**必須由使用者回覆 `/approve <id>` 才會合併生效**。

## 🚀 快速開始

### 1. 建立設定檔
```bash
cp config.toml.example config.toml
```
編輯 `config.toml`，填入您想使用的模型設定（支援 `ollama`、`xai_grok`、`grok_oauth`、`claude`、`gemini`、`gemini_oauth`）以及通訊通道（Telegram `bot_token` / `allowed_user_ids` 或 iMessage `allowed_handles`）。

### 2. 執行自動化安全測試與編譯
```bash
go vet ./...
go test -v -race ./...
go build -o bin/kawa ./cmd/kawa
```

### 3. 啟動 Kawa
```bash
# 使用設定檔啟動（若未啟用 TG/iMessage 或加上 -cli，會自動開啟終端機互動模式）
./bin/kawa -config config.toml -cli
```

## 💬 內建管理指令 (Telegram / iMessage / CLI)
- `/help` — 顯示所有可用管理指令
- `/status` — 查看目前啟用的模型、沙盒狀態、Skills 數量與待審核提案
- `/provider <name>` — 動態切換模型 (`ollama`, `xai_grok`, `grok_oauth`, `claude`, `gemini`, `gemini_oauth`)
- `/pending` — 查看等待人工審核的自我修改提案與 Git Diff
- `/approve <id>` — 核准並安全合併指定的自我進化提案（自動備份並重編譯 `./bin/kawa`）
- `/reject <id>` — 拒絕並清理指定的自我進化提案
- `/reset` — 清除當前對話記憶
