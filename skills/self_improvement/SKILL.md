---
name: self_improvement
description: Kawa 核心自主開發工作流 (SKILL.md)：定義如何將新功能拆解為 TASK.md 原子級微任務 (Micro-Steps)、利用 Go AST 結構大綱精準讀取局部程式碼，並透過 SEARCH/REPLACE 區塊與 macOS Seatbelt 沙盒完成自我進化。
---

# Kawa 原子級微任務自主開發指南 (Atomic Micro-Step Self-Development Skill)

當使用者要求開發新功能、修改既有行為或新增工具時，Kawa 採用 **「先確認規格 ➔ 產生 `TASK.md` 原子微任務 ➔ 逐一執行微任務 ➔ 沙盒驗證」** 的標準流程。

## 1. 為什麼要拆成原子級微任務 (Atomic Micro-Steps)？
- **單一步驟只做一件事**：每個微任務 (`MicroStep`) 只修改 **1 個檔案**中的 **1~2 個結構或函式**（約 15~40 行程式碼），讓 14B 本地模型能在 15~30 秒內快速完成，不會發生思考超時 (Timeout)。
- **局部上下文切片 (AST Context Slicing)**：透過 Go AST 掃描檔案中的 `struct` 與 `func` 行號，每個微任務只擷取目標函式與 `import` 區塊，不塞入整份數百行原始碼。

## 2. `SEARCH/REPLACE` 修補語法規範
修改既有 Go 檔案時，必須遵守以下純淨格式（**不要**在 `<<<<<<< SEARCH` 或 `=======` 內部加上 ```` ```go ```` 符號）：

<<<<<<< SEARCH
// 複製原檔案中要修改處的原始程式碼（幾行即可，需與原檔案完全一致）
=======
// 替換後的新 Go 程式碼
>>>>>>> REPLACE

## 3. 做不完就再拆
一步若寫出空檔、截斷、重複型別、註冊簽章錯誤，或工具編譯通過但呼叫方式錯誤，引擎會把這一步換成更小的步驟再做，直到深度上限。不要把同一步原樣重試三次就放棄，也不要在修復時把檔案清空。

註冊工具是 `action: "register"`，由引擎依照建構子簽章寫入，不交給模型重寫 `custom_tools.go`。

## 4. 常見任務拆解範例
- **範例 A（擴充現有通訊通道，如 Telegram Inline Query）**：
  - `step-1` (`internal/channel/telegram.go`, `focus_symbols: ["tgUpdatesResp", "TelegramChannel"]`)：在 `tgUpdatesResp` 新增 `InlineQuery` 結構欄位，並為 `TelegramChannel` 新增 `answerInlineQuery` 輔助方法。
  - `step-2` (`internal/channel/telegram.go`, `focus_symbols: ["Start"]`)：在 `Start()` 的 `for _, u := range updates.Result` 迴圈中檢查 `u.InlineQuery != nil` 並呼叫處理邏輯。
- **範例 B（新增 Agent 獨立工具 Tool）**：
  - `step-1` (`internal/tools/<name>_tool.go`, `action: "skeleton"`)：只寫型別、建構子、Name、Description、Parameters。Execute 先回傳「尚未實作」。
  - `step-2` (`internal/tools/<name>_tool.go`, `action: "execute"`)：只寫 Execute。
  - `step-3` (`internal/tools/custom_tools.go`, `action: "register"`)：不要手寫 `reg.Register`，交給引擎依簽章註冊。
