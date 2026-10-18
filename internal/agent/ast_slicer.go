package agent

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"regexp"
	"strconv"
	"strings"
)

var compilerErrorLineRe = regexp.MustCompile(`([a-zA-Z0-9_./-]+\.go):(\d+):`)

// goSymbolRange represents a top-level declaration in a Go source file discovered via go/ast.
type goSymbolRange struct {
	Name      string
	Kind      string // "type", "func", "var", "const"
	Signature string
	StartLine int
	EndLine   int
}

// parseGoSymbols dynamically parses any Go file using go/ast and returns its import end line
// and all top-level struct/interface/func/method declarations with exact line ranges.
func parseGoSymbols(src string) (int, []goSymbolRange) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "source.go", src, parser.ParseComments)
	if err != nil {
		return 1, nil
	}

	lines := strings.Split(src, "\n")
	headerEndLine := fset.Position(file.Name.End()).Line

	var symbols []goSymbolRange
	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.GenDecl:
			if d.Tok == token.IMPORT {
				end := fset.Position(d.End()).Line
				if end > headerEndLine {
					headerEndLine = end
				}
				continue
			}
			start := fset.Position(d.Pos()).Line
			end := fset.Position(d.End()).Line
			for _, spec := range d.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					name := s.Name.Name
					sig := firstLineAt(lines, start)
					symbols = append(symbols, goSymbolRange{
						Name:      name,
						Kind:      "type",
						Signature: sig,
						StartLine: start,
						EndLine:   end,
					})
				case *ast.ValueSpec:
					for _, id := range s.Names {
						symbols = append(symbols, goSymbolRange{
							Name:      id.Name,
							Kind:      strings.ToLower(d.Tok.String()),
							Signature: firstLineAt(lines, start),
							StartLine: start,
							EndLine:   end,
						})
					}
				}
			}
		case *ast.FuncDecl:
			start := fset.Position(d.Pos()).Line
			end := fset.Position(d.End()).Line
			name := d.Name.Name
			symbols = append(symbols, goSymbolRange{
				Name:      name,
				Kind:      "func",
				Signature: firstLineAt(lines, start),
				StartLine: start,
				EndLine:   end,
			})
		}
	}
	return headerEndLine, symbols
}

func firstLineAt(lines []string, lineNum int) string {
	if lineNum < 1 || lineNum > len(lines) {
		return ""
	}
	return strings.TrimSpace(lines[lineNum-1])
}

// buildGoFileOutline returns a compact structural outline of a Go source file
// so the LLM can plan atomic micro-steps without reading hundreds of lines of implementation.
func buildGoFileOutline(relPath, src string) string {
	lines := strings.Split(src, "\n")
	_, symbols := parseGoSymbols(src)
	if len(symbols) == 0 {
		return fmt.Sprintf("File: %s (%d lines)\n%s", relPath, len(lines), truncateRunes(src, 1200))
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("File: %s (共 %d 行)\n", relPath, len(lines)))
	for _, sym := range symbols {
		sb.WriteString(fmt.Sprintf("  - [L%d-L%d] %s (symbol: %q)\n", sym.StartLine, sym.EndLine, sym.Signature, sym.Name))
	}
	return sb.String()
}

// sliceGoFileForSymbols extracts ONLY the package+imports header and the specific AST declarations
// needed for the current MicroStep. If the file is short (<= 110 lines) or no symbols match,
// it returns the full file so the model always has valid context.
func sliceGoFileForSymbols(src string, focusSymbols []string, instruction string) string {
	lines := strings.Split(src, "\n")
	if len(lines) <= 110 {
		return src
	}

	headerEnd, symbols := parseGoSymbols(src)
	if len(symbols) == 0 {
		return src
	}

	focusSet := make(map[string]bool)
	for _, fs := range focusSymbols {
		cleaned := strings.ToLower(strings.TrimSpace(fs))
		if cleaned != "" {
			focusSet[cleaned] = true
		}
	}
	lowerInstr := strings.ToLower(instruction)

	var matched []goSymbolRange
	for _, sym := range symbols {
		lowerName := strings.ToLower(sym.Name)
		if focusSet[lowerName] || (len(lowerName) >= 3 && strings.Contains(lowerInstr, lowerName)) {
			matched = append(matched, sym)
		}
	}

	// If nothing matched, return the whole file (up to 4500 runes) so the model isn't blind
	if len(matched) == 0 {
		return truncateRunes(src, 4500)
	}

	var sb strings.Builder
	if headerEnd > len(lines) {
		headerEnd = len(lines)
	}
	sb.WriteString(strings.Join(lines[:headerEnd], "\n"))
	sb.WriteString("\n\n// ... (僅擷取與本微任務相關的區塊) ...\n\n")

	for _, sym := range matched {
		startIdx := sym.StartLine - 1
		if startIdx < headerEnd {
			startIdx = headerEnd
		}
		endIdx := sym.EndLine
		if endIdx > len(lines) {
			endIdx = len(lines)
		}
		sb.WriteString(fmt.Sprintf("// --- [%s: L%d-L%d] ---\n", sym.Name, sym.StartLine, sym.EndLine))
		sb.WriteString(strings.Join(lines[startIdx:endIdx], "\n"))
		sb.WriteString("\n\n")
	}
	return strings.TrimSpace(sb.String()) + "\n"
}

// extractCompilerErrorSnippet parses the first `path/to/file.go:LINE:` in compiler output
// and extracts the file header (package + imports) along with +/- 25 lines around that line
// from workingFiles so the LLM has both global import visibility and localized error context.
func extractCompilerErrorSnippet(compilerErr string, workingFiles map[string]string) (string, string) {
	m := compilerErrorLineRe.FindStringSubmatch(compilerErr)
	if len(m) < 3 {
		return "", ""
	}
	errFile := m[1]
	lineNum, err := strconv.Atoi(m[2])
	if err != nil {
		return "", ""
	}

	var targetPath, src string
	for path, content := range workingFiles {
		if path == errFile || strings.HasSuffix(path, errFile) || strings.HasSuffix(errFile, path) {
			targetPath = path
			src = content
			break
		}
	}
	if targetPath == "" {
		return "", ""
	}

	lines := strings.Split(src, "\n")
	// If the file is short (<= 120 lines), return the entire file for 100% full context
	if len(lines) <= 120 {
		return targetPath, src
	}

	headerEnd, _ := parseGoSymbols(src)
	if headerEnd < 1 || headerEnd > len(lines) {
		headerEnd = 1
	}

	start := lineNum - 25
	if start < 0 {
		start = 0
	}
	end := lineNum + 25
	if end > len(lines) {
		end = len(lines)
	}

	// If the error line is already near the header, return one contiguous chunk from line 0 to end
	if start <= headerEnd {
		return targetPath, strings.Join(lines[:end], "\n")
	}

	// Otherwise, combine the package+import header and the localized error snippet
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("// --- 檔頭與 Import 宣告區塊 (L1-L%d) ---\n", headerEnd))
	sb.WriteString(strings.Join(lines[:headerEnd], "\n"))
	sb.WriteString(fmt.Sprintf("\n\n// ... (中間省略 L%d 至 L%d) ...\n\n", headerEnd+1, start))
	sb.WriteString(fmt.Sprintf("// --- 編譯報錯處局部原始碼 (L%d-L%d) ---\n", start+1, end))
	sb.WriteString(strings.Join(lines[start:end], "\n"))
	return targetPath, sb.String()
}
