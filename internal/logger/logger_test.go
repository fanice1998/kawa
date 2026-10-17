package logger

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"
)

func TestLoggerFormatAndDailyFile(t *testing.T) {
	var buf bytes.Buffer
	tmpLogDir := filepath.Join(t.TempDir(), "logs")
	log, err := Init("info", tmpLogDir, &buf)
	if err != nil {
		t.Fatalf("Init failed: %v", err)
	}

	log.Info().Str("component", "test").Msg("hello kawa")

	out := buf.String()
	// Strip ANSI escape codes if any
	ansiRegex := regexp.MustCompile(`\x1b\[[0-9;]*m`)
	clean := ansiRegex.ReplaceAllString(out, "")

	// Must start with "YYYY-MM-DD HH:mm:ss INF hello kawa"
	pattern := regexp.MustCompile(`^\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2} INF hello kawa`)
	if !pattern.MatchString(clean) {
		t.Fatalf("log output %q does not match expected 'YYYY-MM-DD HH:mm:ss LEVEL msg' format", clean)
	}

	expectedFile := filepath.Join(tmpLogDir, time.Now().Format("2006-01-02")+".log")
	fileBytes, err := os.ReadFile(expectedFile)
	if err != nil {
		t.Fatalf("expected daily log file %s to exist: %v", expectedFile, err)
	}
	if !pattern.MatchString(string(fileBytes)) {
		t.Fatalf("daily log file content %q does not match expected format", string(fileBytes))
	}
}
