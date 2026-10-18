package agent

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/fanice1998/kawa/internal/llm"
	"github.com/rs/zerolog/log"
)

const (
	maxStepDepth   = 6
	maxSubdivides  = 12
	maxNewFileLine = 70
)

type failureKind int

const (
	failureNone failureKind = iota
	failureTruncated
	failureTooLarge
	failureSearchMiss
	failureRedeclared
	failureUndefined
	failureTypeMismatch
	failureUnusable
)

var (
	otherDeclFileRe = regexp.MustCompile(`([A-Za-z0-9_./-]+\.go):\d+:\d+:\s+other declaration of`)
	undefinedNewRe  = regexp.MustCompile(`undefined:\s*(New[A-Za-z0-9_]+)`)
)

func classifyFailure(msg string) failureKind {
	switch {
	case strings.Contains(msg, "unexpected EOF") || strings.Contains(msg, "found 'EOF'") || strings.Contains(msg, "缺少 package") || strings.Contains(msg, "括號沒有閉合") || strings.Contains(msg, "無法解析") || strings.Contains(msg, "空的"):
		return failureTruncated
	case strings.Contains(msg, "超過") && strings.Contains(msg, "行"):
		return failureTooLarge
	case strings.Contains(msg, "redeclared"):
		return failureRedeclared
	case strings.Contains(msg, "undefined:"):
		return failureUndefined
	case strings.Contains(msg, "cannot use") || strings.Contains(msg, "參數型別"):
		return failureTypeMismatch
	case strings.Contains(msg, "SEARCH") || strings.Contains(msg, "REPLACE") || strings.Contains(msg, "未輸出有效"):
		return failureSearchMiss
	case strings.Contains(msg, "尚未實作") || strings.Contains(msg, "SendRich") || strings.Contains(msg, "通道名稱") || strings.Contains(msg, "RegisterCustomTools") || strings.Contains(msg, "沒有產生可註冊"):
		return failureUnusable
	default:
		return failureNone
	}
}

func shouldResumeTask(spec *TaskSpec, extra string) bool {
	if spec == nil || spec.Stage3Done || len(spec.Steps) == 0 {
		return false
	}
	extra = strings.TrimSpace(extra)
	if extra == "" || isResumePhrase(extra) {
		return true
	}
	return spec.OriginInstruction != "" && spec.OriginInstruction == extra
}

func isResumePhrase(extra string) bool {
	extra = strings.TrimSpace(extra)
	if extra == "" || len([]rune(extra)) > 40 {
		return false
	}
	for _, kw := range []string{"繼續", "重試", "接續", "再試", "重新生成", "重新執行", "重跑"} {
		if strings.Contains(extra, kw) {
			return true
		}
	}
	return false
}

func expandOversizedSteps(steps []MicroStep) []MicroStep {
	var out []MicroStep
	for _, st := range steps {
		if st.Done {
			out = append(out, st)
			continue
		}
		st = normalizeRegistrationStep(st)
		if st.Action == "register" || st.Action == "skeleton" || st.Action == "execute" || st.Depth > 0 {
			out = append(out, st)
			continue
		}
		if isWholeToolStep(st) {
			out = append(out, skeletonStep(st), executeStep(st), registerStep(st))
			continue
		}
		out = append(out, st)
	}
	return out
}

func normalizeRegistrationStep(st MicroStep) MicroStep {
	if st.Action == "register" {
		return st
	}
	blob := st.Title + " " + st.Instruction
	if strings.Contains(st.TargetFile, "custom_tools.go") && (strings.Contains(blob, "註冊") || strings.Contains(blob, "Register")) {
		st.Action = "register"
		st.Instruction = "由引擎依照建構子簽章寫入 RegisterCustomTools。"
	}
	return st
}

func isWholeToolStep(st MicroStep) bool {
	if !isToolImplPath(st.TargetFile) || st.Action == "register" || st.Action == "skeleton" || st.Action == "execute" {
		return false
	}
	if st.Action == "create" {
		return true
	}
	blob := st.Title + st.Instruction
	hasBuild := strings.Contains(blob, "建立") || strings.Contains(blob, "新增") || strings.Contains(blob, "實作")
	hasRest := strings.Contains(blob, "註冊") || strings.Contains(blob, "Register") || strings.Contains(blob, "Execute") || strings.Contains(blob, "工具")
	return hasBuild && hasRest
}

func isToolImplPath(p string) bool {
	p = filepath.ToSlash(strings.TrimSpace(p))
	if !strings.Contains(p, "internal/tools/") || !strings.HasSuffix(p, ".go") {
		return false
	}
	base := filepath.Base(p)
	return base != "tool.go" && base != "custom_tools.go"
}

func skeletonStep(parent MicroStep) MicroStep {
	return MicroStep{
		ID:          parent.ID + "-sk",
		Title:       "建立工具骨架",
		TargetFile:  parent.TargetFile,
		Action:      "skeleton",
		Instruction: "只建立工具骨架，不要實作業務邏輯。" + truncateRunes(parent.Instruction, 180),
		Depth:       parent.Depth + 1,
	}
}

func executeStep(parent MicroStep) MicroStep {
	return MicroStep{
		ID:           parent.ID + "-ex",
		Title:        "實作 Execute",
		TargetFile:   parent.TargetFile,
		Action:       "execute",
		FocusSymbols: []string{"Execute"},
		Instruction:  parent.Instruction,
		Depth:        parent.Depth + 1,
	}
}

func registerStep(parent MicroStep) MicroStep {
	return MicroStep{
		ID:          parent.ID + "-reg",
		Title:       "依照建構子簽章註冊工具",
		TargetFile:  "internal/tools/custom_tools.go",
		Action:      "register",
		Instruction: "由引擎依照建構子簽章寫入 RegisterCustomTools。",
		Depth:       parent.Depth + 1,
	}
}

func splitFailedStep(step MicroStep, kind failureKind, detail string, files map[string]string) ([]MicroStep, bool) {
	if step.Depth >= maxStepDepth {
		return nil, false
	}
	switch kind {
	case failureRedeclared:
		existing := existingDeclFile(detail)
		if existing == "" || existing == filepath.ToSlash(step.TargetFile) {
			return []MicroStep{shrinkStep(step, detail)}, true
		}
		mod := step
		mod.ID = step.ID + "-exist"
		mod.Action = "modify"
		mod.TargetFile = existing
		mod.Depth = step.Depth + 1
		mod.Done = false
		mod.Title = "改既有檔案，不新建重複型別"
		mod.Instruction = "型別已經在 " + existing + "。不要建立新檔。只補缺少的方法，不超過 30 行。\n" + truncateRunes(detail, 300)
		return []MicroStep{mod}, true
	case failureUndefined:
		reg := registerStep(step)
		reg.Depth = step.Depth + 1
		if sk, ok := skeletonForUndefined(step, detail); ok {
			return []MicroStep{sk, reg}, true
		}
		return []MicroStep{reg}, true
	case failureTypeMismatch:
		file := toolFileFromDetail(detail, step)
		fix := step
		fix.ID = step.ID + "-sig"
		fix.Action = "modify"
		fix.TargetFile = file
		fix.Depth = step.Depth + 1
		fix.Done = false
		fix.Title = "把建構子改成可註冊的簽章"
		fix.Instruction = truncateRunes(detail, 300) + "\n只改建構子參數，改成接收 *CustomToolDeps。不超過 20 行。"
		reg := registerStep(step)
		reg.Depth = step.Depth + 1
		return []MicroStep{fix, reg}, true
	case failureTruncated, failureTooLarge:
		if step.Action != "skeleton" && step.Action != "execute" && isNewToolTarget(step) {
			return []MicroStep{skeletonStep(step), executeStep(step), registerStep(step)}, true
		}
		if a, b, ok := splitInstruction(step); ok {
			return []MicroStep{a, b}, true
		}
		return []MicroStep{shrinkStep(step, detail)}, true
	case failureSearchMiss:
		if len(step.FocusSymbols) > 1 {
			var kids []MicroStep
			for i, sym := range step.FocusSymbols {
				s := step
				s.ID = fmt.Sprintf("%s-%d", step.ID, i+1)
				s.FocusSymbols = []string{sym}
				s.Depth = step.Depth + 1
				s.Done = false
				s.Title = step.Title + " / " + sym
				s.Instruction = "只修改 " + sym + "。SEARCH 必須逐字複製原文，不超過 25 行。\n" + truncateRunes(step.Instruction, 240)
				kids = append(kids, s)
			}
			return kids, true
		}
		return []MicroStep{shrinkStep(step, detail)}, true
	case failureUnusable:
		if strings.Contains(detail, "沒有在 RegisterCustomTools") {
			reg := registerStep(step)
			reg.Depth = step.Depth + 1
			return []MicroStep{reg}, true
		}
		if strings.Contains(detail, "請建立 ") {
			path := suggestedCreatePath(detail)
			sk := skeletonStep(step)
			sk.TargetFile = path
			sk.ID = step.ID + "-sk"
			sk.Depth = step.Depth + 1
			sk.Instruction = "建立可註冊的工具骨架，型別要有 Name()。Execute 先回傳尚未實作。\n" + truncateRunes(detail, 200)
			ex := executeStep(step)
			ex.TargetFile = path
			ex.Depth = step.Depth + 1
			return []MicroStep{sk, ex, registerStep(step)}, true
		}
		fix := step
		fix.ID = step.ID + "-wire"
		fix.Action = "execute"
		fix.Depth = step.Depth + 1
		fix.Done = false
		fix.Title = "修正工具呼叫方式"
		if !isToolImplPath(fix.TargetFile) {
			fix.TargetFile = firstToolFile(files)
		}
		if fix.TargetFile == "" {
			fix.TargetFile = "internal/tools/evolved_tool.go"
			fix.Action = "skeleton"
		}
		fix.Instruction = truncateRunes(detail, 400) + "\n只改 Execute。Router.Send 的參數是 (ctx, channelName, recipient, text)，通道名稱用 \"telegram\"。送富文本要取出 channel 再呼叫 SendRich。不要改註冊，不超過 40 行。"
		return []MicroStep{fix}, true
	default:
		return []MicroStep{shrinkStep(step, detail)}, true
	}
}

func isNewToolTarget(step MicroStep) bool {
	if !isToolImplPath(step.TargetFile) {
		return false
	}
	if step.Action == "create" || step.Action == "" {
		return true
	}
	blob := step.Title + step.Instruction
	return strings.Contains(blob, "建立") || strings.Contains(blob, "新增") || strings.Contains(blob, "實作")
}

func shrinkStep(step MicroStep, detail string) MicroStep {
	s := step
	s.ID = step.ID + "-s"
	s.Depth = step.Depth + 1
	s.Done = false
	s.Title = step.Title + "（縮小）"
	if s.Action == "create" {
		s.Action = "skeleton"
	}
	s.Instruction = "前一次失敗：" + truncateRunes(detail, 180) + "\n這一次只改一個函式，輸出不超過 25 行。SEARCH 必須逐字複製原文。\n原指示：" + truncateRunes(step.Instruction, 220)
	return s
}

func splitInstruction(step MicroStep) (MicroStep, MicroStep, bool) {
	parts := splitTwo(strings.TrimSpace(step.Instruction))
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return MicroStep{}, MicroStep{}, false
	}
	a, b := step, step
	a.ID, b.ID = step.ID+"a", step.ID+"b"
	a.Title, b.Title = step.Title+"（前半）", step.Title+"（後半）"
	a.Instruction = parts[0] + "\n只做這一半，不超過 30 行。"
	b.Instruction = parts[1] + "\n只做這一半，不超過 30 行。"
	a.Done, b.Done = false, false
	a.Depth, b.Depth = step.Depth+1, step.Depth+1
	return a, b, true
}

func splitTwo(text string) []string {
	if i := strings.Index(text, "\n\n"); i > 0 && i < len(text)-2 {
		return []string{strings.TrimSpace(text[:i]), strings.TrimSpace(text[i:])}
	}
	runes := []rune(text)
	if len(runes) < 40 {
		return nil
	}
	mid := len(runes) / 2
	for delta := 0; delta < 80; delta++ {
		for _, idx := range []int{mid + delta, mid - delta} {
			if idx <= 0 || idx >= len(runes)-1 {
				continue
			}
			switch runes[idx] {
			case '。', '；', '\n', '.', ';':
				left := strings.TrimSpace(string(runes[:idx+1]))
				right := strings.TrimSpace(string(runes[idx+1:]))
				if left != "" && right != "" {
					return []string{left, right}
				}
			}
		}
	}
	return []string{strings.TrimSpace(string(runes[:mid])), strings.TrimSpace(string(runes[mid:]))}
}

func skeletonForUndefined(step MicroStep, detail string) (MicroStep, bool) {
	m := undefinedNewRe.FindStringSubmatch(detail)
	if len(m) < 2 {
		return MicroStep{}, false
	}
	file := step.TargetFile
	if !isToolImplPath(file) {
		file = "internal/tools/missing_tool.go"
	}
	sk := skeletonStep(step)
	sk.ID = step.ID + "-miss"
	sk.TargetFile = file
	sk.Depth = step.Depth + 1
	sk.Instruction = "建立 " + m[1] + "，回傳型別要有 Name()。Execute 先回傳尚未實作。不要註冊。"
	return sk, true
}

func existingDeclFile(detail string) string {
	if m := otherDeclFileRe.FindStringSubmatch(detail); len(m) > 1 {
		return filepath.ToSlash(m[1])
	}
	return ""
}

func compilerFilePath(compilerErr string) string {
	m := compilerErrorLineRe.FindStringSubmatch(compilerErr)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

func toolFileFromDetail(detail string, step MicroStep) string {
	if m := compilerErrorLineRe.FindAllStringSubmatch(detail, -1); len(m) > 0 {
		for _, one := range m {
			p := filepath.ToSlash(one[1])
			if isToolImplPath(p) {
				return p
			}
		}
	}
	if isToolImplPath(step.TargetFile) {
		return step.TargetFile
	}
	return "internal/tools/missing_tool.go"
}

func suggestedCreatePath(detail string) string {
	const key = "請建立 "
	i := strings.Index(detail, key)
	if i < 0 {
		return "internal/tools/evolved_tool.go"
	}
	rest := strings.TrimSpace(detail[i+len(key):])
	field := rest
	if sp := strings.IndexAny(rest, " \n\t"); sp > 0 {
		field = rest[:sp]
	}
	field = strings.Trim(field, "`\"'")
	if strings.HasSuffix(field, ".go") {
		return filepath.ToSlash(field)
	}
	return "internal/tools/evolved_tool.go"
}

func firstToolFile(files map[string]string) string {
	var paths []string
	for p := range files {
		if isToolImplPath(p) {
			paths = append(paths, p)
		}
	}
	sort.Strings(paths)
	if len(paths) == 0 {
		return ""
	}
	return paths[0]
}

func replaceStepAt(steps []MicroStep, i int, children []MicroStep) []MicroStep {
	out := make([]MicroStep, 0, len(steps)+len(children))
	out = append(out, steps[:i]...)
	out = append(out, children...)
	out = append(out, steps[i+1:]...)
	return out
}

func generatedCodeProblem(code string, step MicroStep, previous string) (failureKind, string) {
	if !strings.HasSuffix(step.TargetFile, ".go") {
		if strings.TrimSpace(code) == "" {
			return failureTruncated, "產出是空的"
		}
		return failureNone, ""
	}
	trim := strings.TrimSpace(code)
	if trim == "" || !strings.Contains(trim, "package ") {
		return failureTruncated, "產出是空的或缺少 package 宣告"
	}
	if !parsesGo(trim) {
		return failureTruncated, "產出的 Go 無法解析，輸出可能被截斷"
	}
	lines := strings.Count(trim, "\n") + 1
	if strings.TrimSpace(previous) == "" && lines > maxNewFileLine {
		return failureTooLarge, fmt.Sprintf("這一步寫了 %d 行，超過單步上限 %d 行", lines, maxNewFileLine)
	}
	if strings.TrimSpace(previous) != "" && lines > strings.Count(previous, "\n")+50 {
		return failureTooLarge, fmt.Sprintf("這一步讓檔案增加到 %d 行，單次增加超過 50 行", lines)
	}
	return failureNone, ""
}

func parsesGo(src string) bool {
	if !strings.Contains(src, "package ") {
		return false
	}
	_, err := parser.ParseFile(token.NewFileSet(), "source.go", src, parser.SkipObjectResolution)
	return err == nil
}

func goFileWorse(before, after string) bool {
	after = strings.TrimSpace(after)
	if after == "" || !strings.Contains(after, "package ") {
		return true
	}
	return !parsesGo(after) && parsesGo(before)
}

func purgeEmptyGoFiles(spec *TaskSpec) {
	if spec == nil || spec.WorkingFiles == nil {
		return
	}
	for path, content := range spec.WorkingFiles {
		if strings.HasSuffix(path, ".go") && strings.TrimSpace(content) == "" {
			delete(spec.WorkingFiles, path)
			for i := range spec.Steps {
				if spec.Steps[i].TargetFile == path {
					spec.Steps[i].Done = false
				}
			}
		}
	}
}

func dropRedeclaredFile(files map[string]string, compilerErr string) string {
	other := existingDeclFile(compilerErr)
	first := filepath.ToSlash(compilerFilePath(compilerErr))
	if first == "" || first == other {
		return ""
	}
	for path := range files {
		if path == first || strings.HasSuffix(path, first) || strings.HasSuffix(first, path) {
			if other != "" && (path == other || strings.HasSuffix(other, path) || strings.HasSuffix(path, other)) {
				continue
			}
			delete(files, path)
			return path
		}
	}
	return ""
}

func repairMicroStep(spec *TaskSpec, critique string) MicroStep {
	file := ""
	if spec != nil {
		for _, st := range spec.Steps {
			if isToolImplPath(st.TargetFile) {
				file = st.TargetFile
			}
		}
		if file == "" {
			for _, st := range spec.Steps {
				if strings.HasSuffix(st.TargetFile, ".go") && !strings.Contains(st.TargetFile, "custom_tools.go") {
					file = st.TargetFile
				}
			}
		}
	}
	if file == "" {
		file = "internal/tools/evolved_tool.go"
	}
	n := 1
	if spec != nil && spec.CritiqueRepairs > 0 {
		n = spec.CritiqueRepairs
	}
	return MicroStep{
		ID:          fmt.Sprintf("critique-%d", n),
		Title:       "依照自我審查修正一個問題",
		TargetFile:  file,
		Action:      "modify",
		Instruction: "自我審查指出必須修正的單一問題：" + truncateRunes(critique, 400) + "\n只修這一個問題，不超過 30 行。不要新增第二份相同型別，不要只加註解。",
		Depth:       1,
	}
}

type toolCtor struct {
	File   string
	Name   string
	Params []string
}

func discoverToolConstructors(files map[string]string, steps []MicroStep) []toolCtor {
	allowed := map[string]bool{}
	for _, st := range steps {
		p := filepath.ToSlash(strings.TrimSpace(st.TargetFile))
		if isToolImplPath(p) {
			allowed[p] = true
		}
	}
	var out []toolCtor
	for path, src := range files {
		path = filepath.ToSlash(path)
		if !allowed[path] || strings.TrimSpace(src) == "" {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, src, parser.SkipObjectResolution)
		if err != nil {
			continue
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || !isToolConstructor(fn, file) {
				continue
			}
			var params []string
			if fn.Type.Params != nil {
				for _, field := range fn.Type.Params.List {
					n := 1
					if len(field.Names) > 1 {
						n = len(field.Names)
					}
					ts := typeString(field.Type)
					for i := 0; i < n; i++ {
						params = append(params, ts)
					}
				}
			}
			out = append(out, toolCtor{File: path, Name: fn.Name.Name, Params: params})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func isToolConstructor(fn *ast.FuncDecl, file *ast.File) bool {
	if fn.Recv != nil || fn.Name == nil || !strings.HasPrefix(fn.Name.Name, "New") {
		return false
	}
	if fn.Type.Results == nil || len(fn.Type.Results.List) != 1 {
		return false
	}
	star, ok := fn.Type.Results.List[0].Type.(*ast.StarExpr)
	if !ok {
		return false
	}
	id, ok := star.X.(*ast.Ident)
	if !ok {
		return false
	}
	for _, decl := range file.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Name == nil || fd.Name.Name != "Name" || recvTypeName(fd) != id.Name {
			continue
		}
		return true
	}
	return false
}

func recvTypeName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return ""
	}
	t := fn.Recv.List[0].Type
	if star, ok := t.(*ast.StarExpr); ok {
		t = star.X
	}
	id, ok := t.(*ast.Ident)
	if !ok {
		return ""
	}
	return id.Name
}

func typeString(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.StarExpr:
		return "*" + typeString(t.X)
	case *ast.SelectorExpr:
		return typeString(t.X) + "." + t.Sel.Name
	default:
		return ""
	}
}

func registerExpr(c toolCtor) (string, error) {
	switch {
	case len(c.Params) == 0:
		return fmt.Sprintf("reg.Register(%s())", c.Name), nil
	case len(c.Params) == 1 && c.Params[0] == "*CustomToolDeps":
		return fmt.Sprintf("reg.Register(%s(&deps))", c.Name), nil
	case len(c.Params) == 1 && c.Params[0] == "CustomToolDeps":
		return fmt.Sprintf("reg.Register(%s(deps))", c.Name), nil
	case len(c.Params) == 1 && (c.Params[0] == "*channel.Router" || c.Params[0] == "*Router"):
		return fmt.Sprintf("reg.Register(%s(deps.Router))", c.Name), nil
	default:
		joined := strings.Join(c.Params, ", ")
		if joined == "" {
			joined = "未知"
		}
		return "", fmt.Errorf("%s 的 %s 參數型別為 %s，建構子只能接受 *CustomToolDeps、*channel.Router 或無參數", c.File, c.Name, joined)
	}
}

func syncToolRegistration(files map[string]string, steps []MicroStep) (bool, error) {
	ctors := discoverToolConstructors(files, steps)
	if len(ctors) == 0 {
		return false, nil
	}
	const rel = "internal/tools/custom_tools.go"
	src := files[rel]
	if strings.TrimSpace(src) == "" {
		return false, fmt.Errorf("缺少 %s，無法註冊工具", rel)
	}
	updated := src
	changed := false
	for _, c := range ctors {
		call, err := registerExpr(c)
		if err != nil {
			return false, err
		}
		next, did, err := upsertRegisterCall(updated, c.Name, call, len(c.Params) > 0)
		if err != nil {
			return false, err
		}
		if did {
			changed = true
			updated = next
		}
	}
	if changed {
		files[rel] = updated
	}
	return changed, nil
}

func upsertRegisterCall(src, ctorName, call string, insideRouterIf bool) (string, bool, error) {
	lines := strings.Split(src, "\n")
	for i, line := range lines {
		if isCommentLine(line) {
			continue
		}
		if strings.Contains(line, "reg.Register("+ctorName+"(") {
			if strings.TrimSpace(line) == call {
				return src, false, nil
			}
			lines[i] = leadingWS(line) + call
			return strings.Join(lines, "\n"), true, nil
		}
	}
	funcLine := -1
	for i, line := range lines {
		if strings.Contains(line, "func RegisterCustomTools") {
			funcLine = i
			break
		}
	}
	if funcLine < 0 {
		return "", false, fmt.Errorf("custom_tools.go 找不到 RegisterCustomTools")
	}
	funcEnd := matchingBraceLine(lines, funcLine)
	if funcEnd < 0 {
		return "", false, fmt.Errorf("RegisterCustomTools 括號沒有閉合")
	}
	insertAt := funcEnd
	indent := "\t"
	if insideRouterIf {
		ifLine := -1
		for i := funcLine; i < funcEnd; i++ {
			if strings.Contains(lines[i], "if") && strings.Contains(lines[i], "deps.Router") {
				ifLine = i
				break
			}
		}
		if ifLine >= 0 {
			ifEnd := matchingBraceLine(lines, ifLine)
			if ifEnd > ifLine && ifEnd <= funcEnd {
				insertAt = ifEnd
				indent = "\t\t"
			}
		} else {
			block := []string{"\tif deps.Router != nil {", "\t\t" + call, "\t}"}
			return strings.Join(insertLines(lines, funcEnd, block), "\n"), true, nil
		}
	}
	return strings.Join(insertLines(lines, insertAt, []string{indent + call}), "\n"), true, nil
}

func hasActiveRegister(src, ctorName string) bool {
	for _, line := range strings.Split(src, "\n") {
		if isCommentLine(line) {
			continue
		}
		if strings.Contains(line, "reg.Register("+ctorName+"(") {
			return true
		}
	}
	return false
}

func isCommentLine(line string) bool {
	return strings.HasPrefix(strings.TrimSpace(line), "//")
}

func leadingWS(line string) string {
	i := 0
	for i < len(line) && (line[i] == ' ' || line[i] == '\t') {
		i++
	}
	return line[:i]
}

func matchingBraceLine(lines []string, start int) int {
	depth := 0
	started := false
	for i := start; i < len(lines); i++ {
		for _, r := range lines[i] {
			switch r {
			case '{':
				depth++
				started = true
			case '}':
				depth--
				if started && depth == 0 {
					return i
				}
			}
		}
	}
	return -1
}

func insertLines(lines []string, at int, extra []string) []string {
	if at < 0 {
		at = 0
	}
	if at > len(lines) {
		at = len(lines)
	}
	out := append([]string{}, lines[:at]...)
	out = append(out, extra...)
	out = append(out, lines[at:]...)
	return out
}

func auditToolWiring(files map[string]string, steps []MicroStep, contextText string) error {
	allowed := map[string]bool{}
	for _, st := range steps {
		p := filepath.ToSlash(st.TargetFile)
		if isToolImplPath(p) {
			allowed[p] = true
		}
	}
	var problems []string
	for path := range allowed {
		src := files[path]
		if strings.TrimSpace(src) == "" {
			continue
		}
		problems = append(problems, auditOneToolFile(path, src)...)
	}
	ctors := discoverToolConstructors(files, steps)
	custom := files["internal/tools/custom_tools.go"]
	for _, c := range ctors {
		if !hasActiveRegister(custom, c.Name) {
			problems = append(problems, fmt.Sprintf("%s 的 %s 沒有在 RegisterCustomTools 註冊", c.File, c.Name))
		}
	}
	if len(ctors) == 0 && taskWantsTool(contextText, steps) {
		suggested := "internal/tools/evolved_tool.go"
		for _, st := range steps {
			if strings.HasSuffix(filepath.ToSlash(st.TargetFile), "_tool.go") {
				suggested = filepath.ToSlash(st.TargetFile)
				break
			}
		}
		problems = append(problems, "沒有產生可註冊的 Tool 建構子，請建立 "+suggested)
	}
	if len(problems) == 0 {
		return nil
	}
	sort.Strings(problems)
	return fmt.Errorf("%s", strings.Join(problems, "\n"))
}

func taskWantsTool(contextText string, steps []MicroStep) bool {
	blob := contextText
	for _, st := range steps {
		blob += " " + st.Title + " " + st.Instruction + " " + st.TargetFile
	}
	return strings.Contains(blob, "工具") || strings.Contains(blob, "Tool") || strings.Contains(blob, "_tool.go")
}

func auditOneToolFile(path, src string) []string {
	var problems []string
	if (strings.Contains(src, "富文本") || strings.Contains(src, "富訊息") || strings.Contains(src, "send_rich")) && !strings.Contains(src, "SendRich") {
		problems = append(problems, path+" 要送富文本，但沒有呼叫 SendRich。Router.Send 只送純文字，通道名稱必須是 telegram 這類字串")
	}
	if strings.Contains(src, "尚未實作") {
		problems = append(problems, path+" 的 Execute 仍是尚未實作")
	}
	file, err := parser.ParseFile(token.NewFileSet(), path, src, parser.SkipObjectResolution)
	if err != nil {
		return problems
	}
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || !isRouterSend(call) {
			return true
		}
		if len(call.Args) != 4 {
			problems = append(problems, fmt.Sprintf("%s 的 Router.Send 參數數量是 %d，必須是 (ctx, channelName, recipient, text)", path, len(call.Args)))
			return true
		}
		if name := selectorName(call.Args[1]); isRecipientArgName(name) {
			problems = append(problems, path+" 把接收者 ID 當成通道名稱傳給 Router.Send。通道名稱必須是 \"telegram\" 這類字串，接收者才是使用者 ID")
		}
		return true
	})
	return problems
}

func isRouterSend(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel == nil || sel.Sel.Name != "Send" {
		return false
	}
	switch x := sel.X.(type) {
	case *ast.SelectorExpr:
		return x.Sel != nil && x.Sel.Name == "Router"
	case *ast.Ident:
		return x.Name == "router" || x.Name == "Router"
	default:
		return false
	}
}

func selectorName(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.SelectorExpr:
		if v.Sel == nil {
			return ""
		}
		return v.Sel.Name
	case *ast.Ident:
		return v.Name
	default:
		return ""
	}
}

func isRecipientArgName(name string) bool {
	switch name {
	case "Recipient", "ChatID", "UserID", "SenderID", "Text", "Message":
		return true
	default:
		return false
	}
}

func firstUndone(steps []MicroStep) int {
	for i := range steps {
		if !steps[i].Done {
			return i
		}
	}
	return -1
}

func laterPendingWork(steps []MicroStep, idx int) bool {
	for i := idx + 1; i < len(steps); i++ {
		if !steps[i].Done && steps[i].Action != "register" {
			return true
		}
	}
	return false
}

func stepIndexForFailure(spec *TaskSpec, failure string) int {
	if spec == nil || len(spec.Steps) == 0 {
		return -1
	}
	file := compilerFilePath(failure)
	if file != "" {
		for i := len(spec.Steps) - 1; i >= 0; i-- {
			tf := spec.Steps[i].TargetFile
			if tf == file || strings.HasSuffix(file, tf) || strings.HasSuffix(tf, file) {
				return i
			}
		}
	}
	for i := len(spec.Steps) - 1; i >= 0; i-- {
		if spec.Steps[i].Done {
			return i
		}
	}
	return 0
}

func (a *Agent) ensureToolRegistration(spec *TaskSpec) (bool, error) {
	const rel = "internal/tools/custom_tools.go"
	if spec.WorkingFiles == nil {
		spec.WorkingFiles = map[string]string{}
	}
	if strings.TrimSpace(spec.WorkingFiles[rel]) == "" && a.cfg != nil {
		abs := filepath.Join(a.cfg.Agent.SourceDir, filepath.FromSlash(rel))
		if data, err := os.ReadFile(abs); err == nil {
			spec.WorkingFiles[rel] = string(data)
		}
	}
	return syncToolRegistration(spec.WorkingFiles, spec.Steps)
}

func (a *Agent) executePendingSteps(ctx context.Context, provider llm.Provider, spec *TaskSpec, skillsGuide, channelName, senderID string) (string, bool) {
	purgeEmptyGoFiles(spec)
	for guard := 0; guard < 48; guard++ {
		idx := firstUndone(spec.Steps)
		if idx < 0 {
			return "", true
		}
		step := spec.Steps[idx]
		if step.Action == "register" && laterPendingWork(spec.Steps, idx) {
			reg := spec.Steps[idx]
			spec.Steps = append(append([]MicroStep{}, spec.Steps[:idx]...), spec.Steps[idx+1:]...)
			spec.Steps = append(spec.Steps, reg)
			continue
		}

		a.notifyProgress(ctx, channelName, senderID, fmt.Sprintf(
			"🔨 **[微任務 %d/%d: `%s`]** 正在執行：**%s** (目標檔案: `%s`)...",
			idx+1, len(spec.Steps), step.ID, step.Title, step.TargetFile,
		))

		var stepErr error
		if step.Action == "register" {
			if _, err := a.ensureToolRegistration(spec); err != nil {
				stepErr = err
			} else {
				spec.Steps[idx].Done = true
				spec.UpdatedAt = time.Now().Format("2006-01-02 15:04:05")
				_ = a.writeTaskFiles(renderTaskMarkdown(spec), spec)
				a.notifyProgress(ctx, channelName, senderID, fmt.Sprintf("✅ **[微任務 %d/%d 完成]** `%s` 已依建構子簽章註冊。", idx+1, len(spec.Steps), step.ID))
				continue
			}
		} else {
			for attempt := 1; attempt <= 2; attempt++ {
				code, path, err := a.executeSingleMicroStep(ctx, provider, spec, &step, skillsGuide, stepErr, attempt)
				if err != nil {
					stepErr = err
					log.Warn().Err(err).Str("step_id", step.ID).Int("attempt", attempt).Msg("Atomic micro-step failed")
					continue
				}
				if kind, why := generatedCodeProblem(code, step, spec.WorkingFiles[path]); kind != failureNone {
					stepErr = fmt.Errorf("%s", why)
					log.Warn().Str("step_id", step.ID).Str("reason", why).Msg("Micro-step output rejected as too large or truncated")
					continue
				}
				if spec.WorkingFiles == nil {
					spec.WorkingFiles = map[string]string{}
				}
				spec.WorkingFiles[path] = code
				spec.Steps[idx].Done = true
				spec.UpdatedAt = time.Now().Format("2006-01-02 15:04:05")
				_ = a.writeTaskFiles(renderTaskMarkdown(spec), spec)
				stepErr = nil
				a.notifyProgress(ctx, channelName, senderID, fmt.Sprintf("✅ **[微任務 %d/%d 完成]** `%s` — **%s**", idx+1, len(spec.Steps), step.ID, step.Title))
				break
			}
		}
		if stepErr == nil {
			continue
		}
		if classifyFailure(stepErr.Error()) == failureRedeclared {
			dropRedeclaredFile(spec.WorkingFiles, stepErr.Error())
		}
		if spec.SubdivideCount >= maxSubdivides {
			return stepFailureMessage(step, stepErr), false
		}
		children, ok := splitFailedStep(step, classifyFailure(stepErr.Error()), stepErr.Error(), spec.WorkingFiles)
		if !ok || len(children) == 0 {
			return stepFailureMessage(step, stepErr), false
		}
		spec.SubdivideCount++
		spec.Steps = replaceStepAt(spec.Steps, idx, children)
		spec.Stage2Done = false
		_ = a.writeTaskFiles(renderTaskMarkdown(spec), spec)
		a.notifyProgress(ctx, channelName, senderID, fmt.Sprintf(
			"✂️ 微任務 `%s` 一次做不完（%s）。已再拆成 %d 個更小的步驟，繼續做。",
			step.ID, stepErr.Error(), len(children),
		))
	}
	return "⚠️ 微任務拆分次數達到上限，已停下來保留目前進度。輸入 `/run_task` 會從最小的未完成步驟繼續。", false
}

func stepFailureMessage(step MicroStep, err error) string {
	return fmt.Sprintf(
		"⚠️ 微任務 **%s (%s)** 再拆小之後仍做不完：\n```\n%v\n```\n已完成的步驟都留著。輸入 `/run_task` 會從這個最小步驟繼續，不會整份重來。",
		step.ID, step.Title, err,
	)
}

func (a *Agent) subdivideFromFailure(ctx context.Context, provider llm.Provider, spec *TaskSpec, skillsGuide, channelName, senderID, failure string) (bool, string) {
	if spec.SubdivideCount >= maxSubdivides {
		return false, ""
	}
	idx := stepIndexForFailure(spec, failure)
	if idx < 0 {
		return false, ""
	}
	parentID := spec.Steps[idx].ID
	children, ok := splitFailedStep(spec.Steps[idx], classifyFailure(failure), failure, spec.WorkingFiles)
	if !ok || len(children) == 0 {
		return false, ""
	}
	spec.SubdivideCount++
	spec.Steps = replaceStepAt(spec.Steps, idx, children)
	spec.Stage2Done = false
	spec.Stage3Done = false
	_ = a.writeTaskFiles(renderTaskMarkdown(spec), spec)
	a.notifyProgress(ctx, channelName, senderID, fmt.Sprintf(
		"✂️ `%s` 失敗後再拆成 %d 個更小步驟，繼續做到能完成為止。",
		parentID, len(children),
	))
	msg, done := a.executePendingSteps(ctx, provider, spec, skillsGuide, channelName, senderID)
	if !done {
		return false, msg
	}
	return true, ""
}
