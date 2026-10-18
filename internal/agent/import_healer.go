package agent

import (
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"regexp"
	"sort"
	"strings"
)

// knownPkgImports maps commonly used Go package identifiers to their canonical import paths.
var knownPkgImports = map[string]string{
	"json":      "encoding/json",
	"context":   "context",
	"fmt":       "fmt",
	"strings":   "strings",
	"strconv":   "strconv",
	"time":      "time",
	"sync":      "sync",
	"atomic":    "sync/atomic",
	"os":        "os",
	"io":        "io",
	"bytes":     "bytes",
	"errors":    "errors",
	"filepath":  "path/filepath",
	"path":      "path",
	"http":      "net/http",
	"url":       "net/url",
	"regexp":    "regexp",
	"math":      "math",
	"hex":       "encoding/hex",
	"base64":    "encoding/base64",
	"rand":      "math/rand",
	"bufio":     "bufio",
	"sort":      "sort",
	// Kawa internal packages
	"log":       "github.com/rs/zerolog/log",
	"channel":   "github.com/fanice1998/kawa/internal/channel",
	"config":    "github.com/fanice1998/kawa/internal/config",
	"memory":    "github.com/fanice1998/kawa/internal/memory",
	"sandbox":   "github.com/fanice1998/kawa/internal/sandbox",
	"scheduler": "github.com/fanice1998/kawa/internal/scheduler",
	"evolution": "github.com/fanice1998/kawa/internal/evolution",
	"llm":       "github.com/fanice1998/kawa/internal/llm",
	"tools":     "github.com/fanice1998/kawa/internal/tools",
}

var (
	compilerUndefinedRe  = regexp.MustCompile(`undefined:\s*([a-zA-Z0-9_]+)`)
	compilerUnusedRe     = regexp.MustCompile(`(?:imported and not used:\s*["']?([^"'\s]+)["']?|["']([^"'\s]+)["']\s+imported and not used)`)
)

// AutoHealGoImports analyzes Go source code via AST, discovers used package selectors that lack
// corresponding import statements, and automatically injects them, returning the gofmt-formatted code.
func AutoHealGoImports(src string) (string, bool) {
	if strings.TrimSpace(src) == "" || !strings.Contains(src, "package ") {
		return src, false
	}

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "source.go", src, parser.ParseComments)
	if err != nil {
		return src, false
	}

	existingImports := make(map[string]bool)
	for _, imp := range file.Imports {
		clean := strings.Trim(imp.Path.Value, "\"")
		existingImports[clean] = true
	}

	neededMap := make(map[string]bool)
	ast.Inspect(file, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		id, ok := sel.X.(*ast.Ident)
		if !ok {
			return true
		}
		pkgName := id.Name
		if importPath, ok := knownPkgImports[pkgName]; ok {
			if !existingImports[importPath] {
				neededMap[importPath] = true
			}
		}
		return true
	})

	if len(neededMap) == 0 {
		return src, false
	}

	var needed []string
	for p := range neededMap {
		needed = append(needed, p)
	}
	sort.Strings(needed)

	updated, err := injectImports(src, needed)
	if err != nil {
		return src, false
	}
	return updated, true
}

// CleanUnusedImports parses compiler error output for unused imports and removes them from the source code.
func CleanUnusedImports(src string, compilerErr string) (string, bool) {
	matches := compilerUnusedRe.FindAllStringSubmatch(compilerErr, -1)
	if len(matches) == 0 {
		return src, false
	}

	unusedSet := make(map[string]bool)
	for _, m := range matches {
		target := m[1]
		if target == "" && len(m) > 2 {
			target = m[2]
		}
		target = strings.Trim(target, "\"'; ")
		if target != "" {
			unusedSet[target] = true
		}
	}

	if len(unusedSet) == 0 {
		return src, false
	}

	lines := strings.Split(src, "\n")
	var cleaned []string
	inImport := false
	changed := false

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "import (") {
			inImport = true
			cleaned = append(cleaned, line)
			continue
		}
		if inImport && strings.HasPrefix(trimmed, ")") {
			inImport = false
			cleaned = append(cleaned, line)
			continue
		}
		if inImport {
			shouldRemove := false
			for u := range unusedSet {
				if strings.Contains(line, fmt.Sprintf("%q", u)) || strings.HasSuffix(strings.Trim(trimmed, "\""), "/"+u) || trimmed == u {
					shouldRemove = true
					changed = true
					break
				}
			}
			if shouldRemove {
				continue
			}
		} else if strings.HasPrefix(trimmed, "import ") {
			// Single-line import: import "fmt"
			shouldRemove := false
			for u := range unusedSet {
				if strings.Contains(line, fmt.Sprintf("%q", u)) {
					shouldRemove = true
					changed = true
					break
				}
			}
			if shouldRemove {
				continue
			}
		}
		cleaned = append(cleaned, line)
	}

	if !changed {
		return src, false
	}

	formatted, err := format.Source([]byte(strings.Join(cleaned, "\n")))
	if err != nil {
		return strings.Join(cleaned, "\n"), true
	}
	return string(formatted), true
}

// HealImportsFromCompilerError examines compiler errors for both missing packages (`undefined: xxx`)
// and unused imports, updating affected files in workingFiles in-place. Returns true if any file changed.
func HealImportsFromCompilerError(workingFiles map[string]string, compilerErr string) bool {
	changed := false

	lines := strings.Split(compilerErr, "\n")
	for _, line := range lines {
		m := compilerErrorLineRe.FindStringSubmatch(line)
		if len(m) < 2 {
			continue
		}
		errFile := m[1]

		var targetKey string
		for k := range workingFiles {
			if k == errFile || strings.HasSuffix(k, errFile) || strings.HasSuffix(errFile, k) {
				targetKey = k
				break
			}
		}
		if targetKey == "" {
			continue
		}

		currentSrc := workingFiles[targetKey]

		// 1. Check for `undefined: <pkg>`
		if undMatch := compilerUndefinedRe.FindStringSubmatch(line); len(undMatch) > 1 {
			ident := undMatch[1]
			if pkgPath, ok := knownPkgImports[ident]; ok {
				if updated, err := injectImports(currentSrc, []string{pkgPath}); err == nil && updated != currentSrc {
					workingFiles[targetKey] = updated
					currentSrc = updated
					changed = true
				}
			}
		}

		// 2. Check for unused imports in this line
		if strings.Contains(line, "imported and not used") {
			if cleaned, ok := CleanUnusedImports(currentSrc, line); ok && cleaned != currentSrc {
				workingFiles[targetKey] = cleaned
				changed = true
			}
		}
	}

	return changed
}

// AutoHealAllWorkingFiles runs AutoHealGoImports on all .go files currently present in workingFiles.
func AutoHealAllWorkingFiles(workingFiles map[string]string) bool {
	changed := false
	for path, content := range workingFiles {
		if !strings.HasSuffix(path, ".go") {
			continue
		}
		if healed, ok := AutoHealGoImports(content); ok && healed != content {
			workingFiles[path] = healed
			changed = true
		}
	}
	return changed
}

// injectImports inserts new import paths into Go source code and formats it with gofmt.
func injectImports(src string, importPaths []string) (string, error) {
	if len(importPaths) == 0 {
		return src, nil
	}

	// Filter out any import already literally present
	var needed []string
	for _, p := range importPaths {
		if !strings.Contains(src, fmt.Sprintf("%q", p)) {
			needed = append(needed, p)
		}
	}
	if len(needed) == 0 {
		return src, nil
	}

	var toInsert strings.Builder
	for _, p := range needed {
		toInsert.WriteString(fmt.Sprintf("\t%q\n", p))
	}

	var updated string
	if idx := strings.Index(src, "import ("); idx != -1 {
		insertAt := idx + len("import (\n")
		updated = src[:insertAt] + toInsert.String() + src[insertAt:]
	} else if singleIdx := regexp.MustCompile(`(?m)^import\s+["'][^"']+["']`).FindStringIndex(src); len(singleIdx) == 2 {
		existingSingle := src[singleIdx[0]:singleIdx[1]]
		existingPath := strings.TrimSpace(strings.TrimPrefix(existingSingle, "import"))
		block := fmt.Sprintf("import (\n\t%s\n%s)", existingPath, toInsert.String())
		updated = src[:singleIdx[0]] + block + src[singleIdx[1]:]
	} else {
		// Insert after package declaration
		pkgIdx := regexp.MustCompile(`(?m)^package\s+[a-zA-Z0-9_]+`).FindStringIndex(src)
		if len(pkgIdx) == 2 {
			insertAt := pkgIdx[1]
			block := fmt.Sprintf("\n\nimport (\n%s)\n", toInsert.String())
			updated = src[:insertAt] + block + src[insertAt:]
		} else {
			return src, fmt.Errorf("unable to locate package declaration to inject imports")
		}
	}

	formatted, err := format.Source([]byte(updated))
	if err != nil {
		// If formatting failed because of other syntax errors in the file, return unformatted injection
		return updated, nil
	}
	return string(formatted), nil
}
