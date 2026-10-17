package memory

import (
	"strings"
	"testing"
)

func TestTwoTierMemoryAndMultilingualRAG(t *testing.T) {
	tmpDir := t.TempDir()
	eng, err := NewEngine(tmpDir)
	if err != nil {
		t.Fatalf("NewEngine failed: %v", err)
	}

	// 1. Add Core preference & RAG facts in Traditional Chinese and English
	if _, err := eng.Add("preference", "使用者喜歡使用 Golang 開發 AI Agent，並且非常重視 macOS 沙盒安全", "user"); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Add("fact", "專案的資料庫備份路徑設定在 /Volumes/BackupDrive/postgres", "user"); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Add("fact", "伺服器目前的 Ollama IP 位址為 192.168.191.156:11434，模型是 qwen2.5-coder:14b", "user"); err != nil {
		t.Fatal(err)
	}

	// Verify Core Memory (MEMORY.md) was populated
	core := eng.ReadCoreMemory()
	if !strings.Contains(core, "Golang") {
		t.Fatalf("expected MEMORY.md to contain Golang preference, got: %s", core)
	}

	// 2. Test Chinese RAG query retrieval
	results := eng.Search("請問資料庫備份路徑在哪裡？", 3)
	if len(results) == 0 {
		t.Fatalf("expected RAG search results for Chinese query, got 0")
	}
	if !strings.Contains(results[0].Entry.Content, "/Volumes/BackupDrive/postgres") {
		t.Fatalf("expected top RAG result to match backup path fact, got: %s (score=%.3f)", results[0].Entry.Content, results[0].Score)
	}

	// 3. Test mixed English/Chinese RAG query retrieval
	results2 := eng.Search("Ollama 的 IP 位址是多少", 3)
	if len(results2) == 0 || !strings.Contains(results2[0].Entry.Content, "192.168.191.156") {
		t.Fatalf("expected top RAG result to match Ollama IP, got: %+v", results2)
	}
}
