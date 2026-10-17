package sandbox

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ErrAccessDenied is returned when an operation violates sandbox constraints.
var ErrAccessDenied = errors.New("sandbox access denied")

// protectedPaths lists files/prefixes that the AI agent is prohibited from overwriting directly.
var protectedPaths = []string{
	".git",
	"config.toml",
	".env",
	".kawa",
}

// FSJail wraps Go 1.24+ os.Root to enforce kernel/FD-level directory confinement.
// It deliberately omits any file deletion (Remove/RemoveAll) methods to prevent data loss.
type FSJail struct {
	rootDir   string
	backupDir string
	readOnly  bool
}

// NewFSJail creates a new OS-level directory jail rooted at rootDir.
// If backupDir is non-empty, existing files are backed up before being overwritten.
func NewFSJail(rootDir string, backupDir string, readOnly bool) (*FSJail, error) {
	absRoot, err := filepath.Abs(rootDir)
	if err != nil {
		return nil, fmt.Errorf("resolve root dir: %w", err)
	}
	if err := os.MkdirAll(absRoot, 0o755); err != nil {
		return nil, fmt.Errorf("ensure root dir %s: %w", absRoot, err)
	}
	var absBackup string
	if strings.TrimSpace(backupDir) != "" {
		absBackup, err = filepath.Abs(backupDir)
		if err != nil {
			return nil, fmt.Errorf("resolve backup dir: %w", err)
		}
		if err := os.MkdirAll(absBackup, 0o755); err != nil {
			return nil, fmt.Errorf("ensure backup dir %s: %w", absBackup, err)
		}
	}
	return &FSJail{
		rootDir:   absRoot,
		backupDir: absBackup,
		readOnly:  readOnly,
	}, nil
}

// RootDir returns the absolute path of the jailed root directory.
func (j *FSJail) RootDir() string {
	return j.rootDir
}

// sanitizeRelPath validates that relPath is relative and does not target protected files on write.
func sanitizeRelPath(relPath string, isWrite bool) (string, error) {
	trimmed := strings.TrimSpace(relPath)
	if trimmed == "" || trimmed == "." {
		if isWrite {
			return "", fmt.Errorf("%w: cannot write to root directory itself", ErrAccessDenied)
		}
		return ".", nil
	}
	if filepath.IsAbs(trimmed) {
		return "", fmt.Errorf("%w: absolute paths are forbidden (%s)", ErrAccessDenied, relPath)
	}
	cleaned := filepath.Clean(trimmed)
	if cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: path traversal outside root is forbidden (%s)", ErrAccessDenied, relPath)
	}

	if isWrite {
		for _, p := range protectedPaths {
			if cleaned == p || strings.HasPrefix(cleaned, p+string(filepath.Separator)) {
				return "", fmt.Errorf("%w: path %q is protected and cannot be modified", ErrAccessDenied, cleaned)
			}
		}
	}
	return cleaned, nil
}

// ReadFile safely reads a file inside the jailed root using os.OpenRoot.
func (j *FSJail) ReadFile(relPath string) ([]byte, error) {
	cleaned, err := sanitizeRelPath(relPath, false)
	if err != nil {
		return nil, err
	}

	root, err := os.OpenRoot(j.rootDir)
	if err != nil {
		return nil, fmt.Errorf("open root %s: %w", j.rootDir, err)
	}
	defer root.Close()

	f, err := root.Open(cleaned)
	if err != nil {
		return nil, fmt.Errorf("%w: open %s: %v", ErrAccessDenied, cleaned, err)
	}
	defer f.Close()

	// Limit single file read to 5MB to avoid memory exhaustion
	const maxReadBytes = 5 * 1024 * 1024
	data, err := io.ReadAll(io.LimitReader(f, maxReadBytes))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", cleaned, err)
	}
	return data, nil
}

// WriteFile safely writes content to relPath inside the jailed root using os.OpenRoot.
// If the file already exists and backupDir is configured, a timestamped backup is saved first.
func (j *FSJail) WriteFile(relPath string, content []byte) error {
	if j.readOnly {
		return fmt.Errorf("%w: this jail is read-only", ErrAccessDenied)
	}
	cleaned, err := sanitizeRelPath(relPath, true)
	if err != nil {
		return err
	}

	root, err := os.OpenRoot(j.rootDir)
	if err != nil {
		return fmt.Errorf("open root %s: %w", j.rootDir, err)
	}
	defer root.Close()

	// Ensure parent directories exist inside the root jail
	parent := filepath.Dir(cleaned)
	if parent != "." && parent != "" {
		if err := mkdirAllInRoot(root, parent); err != nil {
			return fmt.Errorf("%w: create parent dir %s: %v", ErrAccessDenied, parent, err)
		}
	}

	// If file already exists and backupDir is set, back it up before overwriting
	if j.backupDir != "" {
		if existing, err := root.Open(cleaned); err == nil {
			oldBytes, readErr := io.ReadAll(existing)
			_ = existing.Close()
			if readErr == nil {
				_ = j.saveBackup(cleaned, oldBytes)
			}
		}
	}

	f, err := root.OpenFile(cleaned, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("%w: write %s: %v", ErrAccessDenied, cleaned, err)
	}
	defer f.Close()

	if _, err := f.Write(content); err != nil {
		return fmt.Errorf("write bytes to %s: %w", cleaned, err)
	}
	return nil
}

// ListDir returns the entries in relPath within the jailed root.
func (j *FSJail) ListDir(relPath string) ([]string, error) {
	cleaned, err := sanitizeRelPath(relPath, false)
	if err != nil {
		return nil, err
	}

	root, err := os.OpenRoot(j.rootDir)
	if err != nil {
		return nil, fmt.Errorf("open root %s: %w", j.rootDir, err)
	}
	defer root.Close()

	entries, err := fs.ReadDir(root.FS(), cleaned)
	if err != nil {
		return nil, fmt.Errorf("%w: readdir %s: %v", ErrAccessDenied, cleaned, err)
	}

	result := make([]string, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() {
			name += "/"
		}
		result = append(result, name)
	}
	return result, nil
}

func mkdirAllInRoot(root *os.Root, relDir string) error {
	parts := strings.Split(filepath.ToSlash(relDir), "/")
	curr := ""
	for _, part := range parts {
		if part == "" || part == "." {
			continue
		}
		if curr == "" {
			curr = part
		} else {
			curr = curr + "/" + part
		}
		err := root.Mkdir(curr, 0o755)
		if err != nil && !errors.Is(err, fs.ErrExist) {
			return err
		}
	}
	return nil
}

func (j *FSJail) saveBackup(cleanedRelPath string, data []byte) error {
	ts := time.Now().Format("20060102-150405.000")
	safeName := strings.ReplaceAll(filepath.ToSlash(cleanedRelPath), "/", "__")
	backupFile := filepath.Join(j.backupDir, fmt.Sprintf("%s.%s.bak", safeName, ts))
	return os.WriteFile(backupFile, data, 0o600)
}
