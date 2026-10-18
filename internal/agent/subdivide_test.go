package agent

import (
	"fmt"
	"strings"
	"testing"
)

func TestClassifyFailureFromRealLogs(t *testing.T) {
	cases := []struct {
		msg  string
		want failureKind
	}{
		{"internal/tools/send_rich_tool.go:226:1: syntax error: unexpected EOF", failureTruncated},
		{"internal/tools/send_rich_message.go:1:2: expected 'package', found 'EOF'", failureTruncated},
		{"SendRichMessageTool redeclared in this block", failureRedeclared},
		{"undefined: NewSendInlineKeyboardTool", failureUndefined},
		{"cannot use deps.Router (variable of type *channel.Router) as *CustomToolDeps", failureTypeMismatch},
		{"未找到任何有效的 SEARCH/REPLACE 區塊", failureSearchMiss},
		{"foo_tool.go 的 Execute 仍是尚未實作", failureUnusable},
		{"把接收者 ID 當成通道名稱傳給 Router.Send", failureUnusable},
	}
	for _, tc := range cases {
		if got := classifyFailure(tc.msg); got != tc.want {
			t.Errorf("classify(%q)=%v want %v", tc.msg, got, tc.want)
		}
	}
}

func TestExpandToolCreateIsIdempotent(t *testing.T) {
	steps := []MicroStep{{
		ID:          "step-1",
		Title:       "建立匯率工具",
		TargetFile:  "internal/tools/fx_tool.go",
		Action:      "create",
		Instruction: "建立完整工具並註冊",
	}, {
		ID:          "step-2",
		Title:       "註冊工具",
		TargetFile:  "internal/tools/custom_tools.go",
		Action:      "modify",
		Instruction: "在 RegisterCustomTools 註冊",
	}}
	once := expandOversizedSteps(steps)
	twice := expandOversizedSteps(once)
	if len(once) != 4 {
		t.Fatalf("expected skeleton + execute + register + normalized register, got %#v", once)
	}
	if once[0].Action != "skeleton" || once[1].Action != "execute" || once[2].Action != "register" || once[3].Action != "register" {
		t.Fatalf("unexpected actions: %#v", once)
	}
	if len(twice) != len(once) || twice[0].ID != once[0].ID {
		t.Fatalf("expand should be idempotent, got %#v", twice)
	}
}

func TestSplitStopsAtDepthAndRedeclareRetargets(t *testing.T) {
	deep := MicroStep{ID: "step-1", Depth: maxStepDepth, TargetFile: "internal/tools/a_tool.go", Action: "create"}
	if _, ok := splitFailedStep(deep, failureTooLarge, "超過 90 行", nil); ok {
		t.Fatal("expected split to stop at max depth")
	}

	errText := "internal/tools/send_rich_message.go:11:6: SendRichMessageTool redeclared in this block\n\tinternal/tools/rich_message_tool.go:10:6: other declaration of SendRichMessageTool"
	kids, ok := splitFailedStep(MicroStep{
		ID:         "step-1",
		TargetFile: "internal/tools/send_rich_message.go",
		Action:     "create",
	}, failureRedeclared, errText, nil)
	if !ok || len(kids) != 1 || kids[0].TargetFile != "internal/tools/rich_message_tool.go" || kids[0].Action != "modify" {
		t.Fatalf("expected retarget onto the existing file, got %#v", kids)
	}

	files := map[string]string{
		"internal/tools/send_rich_message.go": "package tools\n",
		"internal/tools/rich_message_tool.go": "package tools\n",
	}
	dropped := dropRedeclaredFile(files, errText)
	if dropped != "internal/tools/send_rich_message.go" {
		t.Fatalf("dropped %q", dropped)
	}
	if _, ok := files[dropped]; ok {
		t.Fatal("duplicate file should be removed from the working set")
	}
	if _, ok := files["internal/tools/rich_message_tool.go"]; !ok {
		t.Fatal("existing file must stay")
	}
}

func TestShouldResumeSameMilestone(t *testing.T) {
	spec := &TaskSpec{
		OriginInstruction: "做匯率工具",
		Steps:             []MicroStep{{ID: "step-1"}},
	}
	if !shouldResumeTask(spec, "做匯率工具") {
		t.Fatal("same instruction should resume")
	}
	if !shouldResumeTask(spec, "繼續") {
		t.Fatal("short resume phrase should resume")
	}
	if shouldResumeTask(spec, "改去做另一個完全不同的功能，請重新規劃整份任務內容") {
		t.Fatal("a different long instruction should redraft")
	}
	spec.Stage3Done = true
	if shouldResumeTask(spec, "做匯率工具") {
		t.Fatal("finished task should not resume")
	}
}

func TestSyncRegistrationFixesWrongSignature(t *testing.T) {
	tool := `package tools
type FooTool struct{}
func NewFooTool(deps *CustomToolDeps) *FooTool { return &FooTool{} }
func (t *FooTool) Name() string { return "foo" }
`
	custom := `package tools
func RegisterCustomTools(reg *Registry, deps CustomToolDeps) {
	if deps.Router != nil {
		reg.Register(NewFooTool(deps.Router))
	}
}
`
	files := map[string]string{
		"internal/tools/foo_tool.go":     tool,
		"internal/tools/custom_tools.go": custom,
	}
	steps := []MicroStep{{TargetFile: "internal/tools/foo_tool.go", Action: "execute"}}
	changed, err := syncToolRegistration(files, steps)
	if err != nil {
		t.Fatal(err)
	}
	if !changed || !strings.Contains(files["internal/tools/custom_tools.go"], "reg.Register(NewFooTool(&deps))") {
		t.Fatalf("signature was not repaired:\n%s", files["internal/tools/custom_tools.go"])
	}
	if strings.Contains(files["internal/tools/custom_tools.go"], "NewFooTool(deps.Router)") {
		t.Fatal("old call should be gone")
	}
	changed, err = syncToolRegistration(files, steps)
	if err != nil || changed {
		t.Fatalf("second sync changed=%v err=%v", changed, err)
	}
}

func TestSyncRegistrationInsertsMissingCall(t *testing.T) {
	tool := `package tools
type BarTool struct{ router *channel.Router }
func NewBarTool(router *channel.Router) *BarTool { return &BarTool{router: router} }
func (t *BarTool) Name() string { return "bar" }
`
	custom := `package tools
func RegisterCustomTools(reg *Registry, deps CustomToolDeps) {
	// reg.Register(NewBarTool(deps.Router))
}
`
	files := map[string]string{
		"internal/tools/bar_tool.go":     tool,
		"internal/tools/custom_tools.go": custom,
	}
	steps := []MicroStep{{TargetFile: "internal/tools/bar_tool.go"}}
	if _, err := syncToolRegistration(files, steps); err != nil {
		t.Fatal(err)
	}
	got := files["internal/tools/custom_tools.go"]
	if !strings.Contains(got, "\tif deps.Router != nil {") || !strings.Contains(got, "reg.Register(NewBarTool(deps.Router))") {
		t.Fatalf("missing mechanical register:\n%s", got)
	}
	active := 0
	for _, line := range strings.Split(got, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		if strings.Contains(line, "reg.Register(NewBarTool") {
			active++
		}
	}
	if active != 1 {
		t.Fatalf("expected one active registration, got %d\n%s", active, got)
	}
}

func TestAuditRejectsRecipientAsChannel(t *testing.T) {
	src := `package tools
func (t *SendRichMessageTool) Execute(ctx context.Context, rawArgs string) (string, error) {
	return t.deps.Router.Send(ctx, args.Recipient, args.Text, "")
}
`
	files := map[string]string{"internal/tools/rich_message_tool.go": src}
	steps := []MicroStep{{TargetFile: "internal/tools/rich_message_tool.go", Done: true}}
	err := auditToolWiring(files, steps, "送出富訊息")
	if err == nil || !strings.Contains(err.Error(), "通道名稱") {
		t.Fatalf("expected wiring audit to fail, got %v", err)
	}
}

func TestGeneratedCodeProblemAndHealGuard(t *testing.T) {
	step := MicroStep{TargetFile: "internal/tools/a_tool.go", Action: "create"}
	if kind, _ := generatedCodeProblem("", step, ""); kind != failureTruncated {
		t.Fatalf("empty file kind=%v", kind)
	}
	if kind, _ := generatedCodeProblem("package tools\nfunc main() {\n", step, ""); kind != failureTruncated {
		t.Fatalf("unclosed file kind=%v", kind)
	}
	var b strings.Builder
	b.WriteString("package tools\n")
	for i := 0; i < 80; i++ {
		fmt.Fprintf(&b, "var x%d int\n", i)
	}
	if kind, _ := generatedCodeProblem(b.String(), step, ""); kind != failureTooLarge {
		t.Fatalf("huge new file kind=%v", kind)
	}
	good := "package tools\nfunc F() int { return 1 }\n"
	if !goFileWorse(good, "") || !goFileWorse(good, "func only") {
		t.Fatal("heal guard should reject an emptied or packageless file")
	}
	if goFileWorse(good, good) {
		t.Fatal("identical valid file is not worse")
	}
}
