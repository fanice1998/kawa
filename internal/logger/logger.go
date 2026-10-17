package logger

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

// TimeFormat strictly enforces "YYYY-MM-DD HH:mm:ss" at the beginning of each log line.
const TimeFormat = "2006-01-02 15:04:05"

// DailyFileWriter writes logs to "<logDir>/YYYY-MM-DD.log" and automatically rotates when the date changes.
type DailyFileWriter struct {
	mu          sync.Mutex
	logDir      string
	currentDate string
	file        *os.File
}

func NewDailyFileWriter(logDir string) (*DailyFileWriter, error) {
	absDir, err := filepath.Abs(logDir)
	if err != nil {
		return nil, fmt.Errorf("resolve log dir %s: %w", logDir, err)
	}
	if err := os.MkdirAll(absDir, 0o755); err != nil {
		return nil, fmt.Errorf("create log dir %s: %w", absDir, err)
	}
	w := &DailyFileWriter{logDir: absDir}
	if err := w.rotateIfNeeded(time.Now()); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *DailyFileWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if err := w.rotateIfNeeded(time.Now()); err != nil {
		return 0, err
	}
	return w.file.Write(p)
}

func (w *DailyFileWriter) rotateIfNeeded(now time.Time) error {
	dateStr := now.Format("2006-01-02")
	if w.file != nil && w.currentDate == dateStr {
		return nil
	}
	if w.file != nil {
		_ = w.file.Close()
		w.file = nil
	}
	filePath := filepath.Join(w.logDir, dateStr+".log")
	f, err := os.OpenFile(filePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("open daily log file %s: %w", filePath, err)
	}
	w.file = f
	w.currentDate = dateStr
	return nil
}

// Init initializes the global zerolog logger and returns a configured zerolog.Logger.
// If logDirOrPath is non-empty, logs are written to `<logDir>/YYYY-MM-DD.log`.
// Output format: "YYYY-MM-DD HH:mm:ss LEVEL message key=val"
func Init(levelStr string, logDirOrPath string, out io.Writer) (zerolog.Logger, error) {
	zerolog.TimeFieldFormat = TimeFormat

	level, err := zerolog.ParseLevel(strings.ToLower(strings.TrimSpace(levelStr)))
	if err != nil || levelStr == "" {
		level = zerolog.InfoLevel
	}
	zerolog.SetGlobalLevel(level)

	if out == nil {
		out = os.Stdout
	}

	var writers []io.Writer
	consoleWriter := NewFormattedWriter(out, true)
	writers = append(writers, consoleWriter)

	target := strings.TrimSpace(logDirOrPath)
	if target != "" {
		// If someone passed an old ".log" file path, use its parent directory or "./logs"
		logDir := target
		if strings.HasSuffix(strings.ToLower(target), ".log") {
			logDir = filepath.Dir(target)
		}
		dailyWriter, err := NewDailyFileWriter(logDir)
		if err != nil {
			return zerolog.Logger{}, err
		}
		fileWriter := NewFormattedWriter(dailyWriter, false)
		writers = append(writers, fileWriter)
	}

	multi := zerolog.MultiLevelWriter(writers...)
	l := zerolog.New(multi).With().Timestamp().Logger()
	log.Logger = l
	return l, nil
}

// NewFormattedWriter creates a zerolog.ConsoleWriter with strict part ordering:
// 1. Timestamp ("YYYY-MM-DD HH:mm:ss")
// 2. Log Level (e.g. INF, WRN, ERR, DBG)
// 3. Message
func NewFormattedWriter(out io.Writer, useColor bool) zerolog.ConsoleWriter {
	return zerolog.ConsoleWriter{
		Out:        out,
		NoColor:    !useColor,
		TimeFormat: TimeFormat,
		PartsOrder: []string{
			zerolog.TimestampFieldName,
			zerolog.LevelFieldName,
			zerolog.MessageFieldName,
		},
		FormatTimestamp: func(i any) string {
			if s, ok := i.(string); ok {
				return s
			}
			return fmt.Sprintf("%v", i)
		},
		FormatLevel: func(i any) string {
			lvl := strings.ToUpper(fmt.Sprintf("%v", i))
			switch lvl {
			case "DEBUG":
				return "DBG"
			case "INFO":
				return "INF"
			case "WARN":
				return "WRN"
			case "ERROR":
				return "ERR"
			case "FATAL":
				return "FTL"
			default:
				return lvl
			}
		},
	}
}
