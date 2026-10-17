package memory

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
)

// Entry represents a single searchable memory item in the RAG store.
type Entry struct {
	ID        string    `json:"id"`
	Category  string    `json:"category"` // "fact", "preference", "episodic", "task"
	Content   string    `json:"content"`
	Source    string    `json:"source"`
	CreatedAt time.Time `json:"created_at"`
	Tokens    map[string]float64 `json:"tokens"`
}

// ScoredEntry holds a retrieved memory entry and its RAG relevance score.
type ScoredEntry struct {
	Entry Entry
	Score float64
}

// Engine implements Hermes-inspired Two-Tier Memory:
// 1. Core Memory (`MEMORY.md`): Always-in-context high-level facts and user preferences.
// 2. Hybrid RAG Store (`rag_store.json`): Searchable episodic & semantic memory using
//    multilingual CJK character bigrams + word TF-IDF cosine similarity and BM25 keyword scoring.
type Engine struct {
	mu           sync.RWMutex
	memDir       string
	coreFilePath string
	ragFilePath  string
	entries      []Entry
}

func NewEngine(stateDir string) (*Engine, error) {
	memDir := filepath.Join(stateDir, "memory")
	if err := os.MkdirAll(memDir, 0o755); err != nil {
		return nil, fmt.Errorf("create memory dir: %w", err)
	}
	e := &Engine{
		memDir:       memDir,
		coreFilePath: filepath.Join(memDir, "MEMORY.md"),
		ragFilePath:  filepath.Join(memDir, "rag_store.json"),
	}
	if err := e.load(); err != nil {
		return nil, err
	}
	return e, nil
}

// ReadCoreMemory returns the contents of MEMORY.md (always-active core facts).
func (e *Engine) ReadCoreMemory() string {
	e.mu.RLock()
	defer e.mu.RUnlock()

	data, err := os.ReadFile(e.coreFilePath)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// UpsertCoreMemory appends or updates core persistent facts in MEMORY.md.
func (e *Engine) UpsertCoreMemory(fact string) error {
	fact = strings.TrimSpace(fact)
	if fact == "" {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()

	existing, _ := os.ReadFile(e.coreFilePath)
	lines := strings.Split(string(existing), "\n")
	for _, l := range lines {
		if strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(l), "- ")) == fact {
			return nil // already exists
		}
	}

	var sb strings.Builder
	if len(strings.TrimSpace(string(existing))) == 0 {
		sb.WriteString("# Kawa Core Memory (MEMORY.md)\n\n")
	} else {
		sb.WriteString(strings.TrimRight(string(existing), "\n") + "\n")
	}
	sb.WriteString(fmt.Sprintf("- [%s] %s\n", time.Now().Format("2006-01-02"), fact))
	return os.WriteFile(e.coreFilePath, []byte(sb.String()), 0o644)
}

// Add stores a new memory chunk in the RAG index and persists it to disk.
// If category == "core", it also adds it to MEMORY.md.
func (e *Engine) Add(category, content, source string) (*Entry, error) {
	content = strings.TrimSpace(content)
	if content == "" {
		return nil, fmt.Errorf("memory content cannot be empty")
	}
	if category == "" {
		category = "fact"
	}
	if strings.EqualFold(category, "core") || strings.EqualFold(category, "preference") {
		_ = e.UpsertCoreMemory(content)
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	// Deduplicate identical recent content
	for i := range e.entries {
		if e.entries[i].Content == content {
			e.entries[i].CreatedAt = time.Now()
			_ = e.saveLocked()
			return &e.entries[i], nil
		}
	}

	entry := Entry{
		ID:        fmt.Sprintf("mem-%d", time.Now().UnixNano()/1e6),
		Category:  category,
		Content:   content,
		Source:    source,
		CreatedAt: time.Now(),
		Tokens:    tokenizeMultilingual(content),
	}
	e.entries = append(e.entries, entry)

	// Cap RAG store at 2000 entries (evict oldest episodic first if exceeded)
	if len(e.entries) > 2000 {
		e.entries = e.entries[len(e.entries)-2000:]
	}

	if err := e.saveLocked(); err != nil {
		return nil, err
	}
	return &entry, nil
}

// RecordEpisode automatically records an important user/assistant interaction into episodic RAG memory.
func (e *Engine) RecordEpisode(channelAndSender, userText, assistantText string) {
	u := strings.TrimSpace(userText)
	a := strings.TrimSpace(assistantText)
	if len([]rune(u)) < 4 || strings.HasPrefix(u, "/") {
		return
	}
	// Never pollute long-term RAG memory with error messages or capability refusals
	badReplies := []string{"無法執行網路查詢", "無法上網", "執行發生錯誤", "(模型未回傳文字內容)", "已達到單次對話最大工具"}
	for _, bad := range badReplies {
		if strings.Contains(a, bad) {
			return
		}
	}
	if len([]rune(a)) > 300 {
		r := []rune(a)
		a = string(r[:300]) + "..."
	}
	episode := fmt.Sprintf("User: %s\nAssistant: %s", u, a)
	_, _ = e.Add("episodic", episode, channelAndSender)
}

// Search performs Hybrid RAG retrieval (Cosine Similarity on TF-IDF n-gram vectors + IDF weighting + recency boost).
func (e *Engine) Search(query string, topK int) []ScoredEntry {
	e.mu.RLock()
	defer e.mu.RUnlock()

	if len(e.entries) == 0 || strings.TrimSpace(query) == "" {
		return nil
	}
	if topK <= 0 {
		topK = 5
	}

	qTokens := tokenizeMultilingual(query)
	if len(qTokens) == 0 {
		return nil
	}

	// Compute Inverse Document Frequency (IDF) across the memory corpus
	docCount := float64(len(e.entries))
	df := make(map[string]float64, len(qTokens))
	for tok := range qTokens {
		for _, entry := range e.entries {
			if entry.Tokens[tok] > 0 {
				df[tok]++
			}
		}
	}

	idf := make(map[string]float64, len(qTokens))
	var qNorm float64
	for tok, qtf := range qTokens {
		w := math.Log(1.0 + (docCount+1.0)/(df[tok]+1.0))
		idf[tok] = w
		wq := qtf * w
		qNorm += wq * wq
	}
	qNorm = math.Sqrt(qNorm)
	if qNorm == 0 {
		return nil
	}

	var scored []ScoredEntry
	now := time.Now()
	for _, entry := range e.entries {
		var dot, dNorm float64
		for tok, dtf := range entry.Tokens {
			w := idf[tok]
			if w == 0 {
				w = 1.0
			}
			wd := dtf * w
			dNorm += wd * wd
			if qtf, ok := qTokens[tok]; ok {
				dot += (qtf * idf[tok]) * wd
			}
		}
		if dot == 0 || dNorm == 0 {
			continue
		}
		cosine := dot / (qNorm * math.Sqrt(dNorm))

		// Boost explicit facts/preferences slightly over auto-logged episodic turns
		categoryBoost := 1.0
		if entry.Category == "fact" || entry.Category == "preference" || entry.Category == "core" {
			categoryBoost = 1.25
		}
		// Mild recency boost
		hoursAge := now.Sub(entry.CreatedAt).Hours()
		recencyBoost := 1.0 / (1.0 + 0.002*math.Max(0, hoursAge))

		finalScore := cosine * categoryBoost * recencyBoost
		if finalScore >= 0.08 {
			scored = append(scored, ScoredEntry{
				Entry: entry,
				Score: finalScore,
			})
		}
	}

	sort.Slice(scored, func(i, j int) bool {
		return scored[i].Score > scored[j].Score
	})

	if len(scored) > topK {
		scored = scored[:topK]
	}
	return scored
}

// ListRecent returns the most recent N memory entries.
func (e *Engine) ListRecent(limit int) []Entry {
	e.mu.RLock()
	defer e.mu.RUnlock()

	if limit <= 0 || limit > len(e.entries) {
		limit = len(e.entries)
	}
	out := make([]Entry, 0, limit)
	for i := len(e.entries) - 1; i >= len(e.entries)-limit; i-- {
		out = append(out, e.entries[i])
	}
	return out
}

// Delete removes a memory entry by ID.
func (e *Engine) Delete(id string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()

	for i, entry := range e.entries {
		if entry.ID == id {
			e.entries = append(e.entries[:i], e.entries[i+1:]...)
			_ = e.saveLocked()
			return true
		}
	}
	return false
}

// Count returns total RAG memory entries.
func (e *Engine) Count() int {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return len(e.entries)
}

func (e *Engine) load() error {
	data, err := os.ReadFile(e.ragFilePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var loaded []Entry
	if err := json.Unmarshal(data, &loaded); err != nil {
		return nil
	}
	for i := range loaded {
		if len(loaded[i].Tokens) == 0 {
			loaded[i].Tokens = tokenizeMultilingual(loaded[i].Content)
		}
	}
	e.entries = loaded
	return nil
}

func (e *Engine) saveLocked() error {
	data, err := json.MarshalIndent(e.entries, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(e.ragFilePath, data, 0o644)
}

// tokenizeMultilingual extracts normalized English/number words AND Chinese/CJK character unigrams + bigrams
// so RAG retrieval works accurately on both Traditional Chinese and English without external CGo dependencies.
func tokenizeMultilingual(text string) map[string]float64 {
	tokens := make(map[string]float64)
	lower := strings.ToLower(text)

	var wordBuf strings.Builder
	var cjkRunes []rune

	flushWord := func() {
		if wordBuf.Len() > 1 {
			w := wordBuf.String()
			tokens[w] += 1.0
		}
		wordBuf.Reset()
	}

	for _, r := range lower {
		if unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) || unicode.Is(unicode.Katakana, r) {
			flushWord()
			cjkRunes = append(cjkRunes, r)
			tokens[string(r)] += 0.5
		} else if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-' {
			wordBuf.WriteRune(r)
		} else {
			flushWord()
			if len(cjkRunes) > 1 {
				for i := 0; i < len(cjkRunes)-1; i++ {
					bigram := string(cjkRunes[i : i+2])
					tokens[bigram] += 1.5
				}
			}
			cjkRunes = cjkRunes[:0]
		}
	}
	flushWord()
	if len(cjkRunes) > 1 {
		for i := 0; i < len(cjkRunes)-1; i++ {
			bigram := string(cjkRunes[i : i+2])
			tokens[bigram] += 1.5
		}
	}

	// Sub-linear TF scaling (1 + log(tf))
	for k, v := range tokens {
		if v > 0 {
			tokens[k] = 1.0 + math.Log(v)
		}
	}
	return tokens
}
