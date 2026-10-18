package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/fanice1998/kawa/internal/scheduler"
)

type contextKey string

const (
	ctxKeyChannel   contextKey = "channel"
	ctxKeyRecipient contextKey = "recipient"
	ctxKeyMsgID     contextKey = "message_id"
	ctxKeyReplyID   contextKey = "reply_to_message_id"
)

// WithDeliveryTarget attaches the active channel & sender ID to ctx so scheduled jobs know where to deliver results.
func WithDeliveryTarget(ctx context.Context, channelName, recipient string) context.Context {
	ctx = context.WithValue(ctx, ctxKeyChannel, channelName)
	ctx = context.WithValue(ctx, ctxKeyRecipient, recipient)
	return ctx
}

// DeliveryTargetFromContext extracts the active channel & sender ID from ctx.
func DeliveryTargetFromContext(ctx context.Context) (string, string) {
	ch, _ := ctx.Value(ctxKeyChannel).(string)
	rec, _ := ctx.Value(ctxKeyRecipient).(string)
	return ch, rec
}

// WithMessageMeta attaches the current message_id and reply_to_message_id to ctx so channel tools can act on them.
func WithMessageMeta(ctx context.Context, messageID, replyToMessageID int) context.Context {
	ctx = context.WithValue(ctx, ctxKeyMsgID, messageID)
	ctx = context.WithValue(ctx, ctxKeyReplyID, replyToMessageID)
	return ctx
}

// MessageMetaFromContext returns (messageID, replyToMessageID) from ctx.
func MessageMetaFromContext(ctx context.Context) (int, int) {
	msgID, _ := ctx.Value(ctxKeyMsgID).(int)
	replyID, _ := ctx.Value(ctxKeyReplyID).(int)
	return msgID, replyID
}

// ScheduleCreateTool creates a recurring or one-shot scheduled task.
type ScheduleCreateTool struct {
	sched *scheduler.Scheduler
}

func NewScheduleCreateTool(sched *scheduler.Scheduler) *ScheduleCreateTool {
	return &ScheduleCreateTool{sched: sched}
}

func (t *ScheduleCreateTool) Name() string { return "schedule_create" }
func (t *ScheduleCreateTool) Description() string {
	return "Create a scheduled automation or reminder. Supports '@once 10m', '@every 30m', '@daily 09:00', or 5-field cron '*/15 * * * *'. Mode 'agent_task' wakes up Kawa to run tools autonomously and send the report; 'direct_message' sends a reminder text directly."
}
func (t *ScheduleCreateTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"name": {
				"type": "string",
				"description": "Human-readable name of the scheduled task."
			},
			"spec": {
				"type": "string",
				"description": "Schedule expression: '@once 10m', '@every 1h', '@daily 09:00', or 5-field cron like '0 9 * * *'."
			},
			"mode": {
				"type": "string",
				"enum": ["agent_task", "direct_message"],
				"description": "'agent_task' (default) runs an autonomous Agent tool loop when triggered; 'direct_message' sends text directly."
			},
			"prompt": {
				"type": "string",
				"description": "Instruction for Kawa to execute (if agent_task) or the reminder message to send (if direct_message)."
			}
		},
		"required": ["name", "spec", "prompt"]
	}`)
}

func (t *ScheduleCreateTool) Execute(ctx context.Context, rawArgs string) (string, error) {
	var args struct {
		Name   string `json:"name"`
		Spec   string `json:"spec"`
		Mode   string `json:"mode"`
		Prompt string `json:"prompt"`
	}
	if err := json.Unmarshal([]byte(rawArgs), &args); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	ch, rec := DeliveryTargetFromContext(ctx)
	if ch == "" {
		ch = "cli"
		rec = "local-admin"
	}
	mode := scheduler.ModeAgentTask
	if strings.EqualFold(args.Mode, "direct_message") {
		mode = scheduler.ModeDirectMessage
	}

	job, err := t.sched.AddJob(args.Name, args.Spec, mode, args.Prompt, ch, rec)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("Scheduled job created!\nID: %s\nName: %s\nSpec: %s\nMode: %s\nNext Run: %s\nTarget: %s (%s)",
		job.ID, job.Name, job.Spec, job.Mode, job.NextRunAt.Format("2006-01-02 15:04:05"), job.Channel, job.Recipient), nil
}

// ScheduleListTool lists all active scheduled tasks.
type ScheduleListTool struct {
	sched *scheduler.Scheduler
}

func NewScheduleListTool(sched *scheduler.Scheduler) *ScheduleListTool {
	return &ScheduleListTool{sched: sched}
}

func (t *ScheduleListTool) Name() string { return "schedule_list" }
func (t *ScheduleListTool) Description() string {
	return "List all currently active scheduled tasks, cron jobs, and timers."
}
func (t *ScheduleListTool) Parameters() json.RawMessage {
	return json.RawMessage(`{"type": "object", "properties": {}}`)
}

func (t *ScheduleListTool) Execute(_ context.Context, _ string) (string, error) {
	jobs := t.sched.ListJobs()
	if len(jobs) == 0 {
		return "No active scheduled jobs.", nil
	}
	var sb strings.Builder
	for i, j := range jobs {
		sb.WriteString(fmt.Sprintf("%d. [%s] %s (spec=%q, mode=%s, next=%s, runs=%d)\n   Prompt: %s\n",
			i+1, j.ID, j.Name, j.Spec, j.Mode, j.NextRunAt.Format("2006-01-02 15:04:05"), j.RunCount, j.Prompt))
	}
	return strings.TrimSpace(sb.String()), nil
}

// ScheduleDeleteTool cancels and removes a scheduled job by ID.
type ScheduleDeleteTool struct {
	sched *scheduler.Scheduler
}

func NewScheduleDeleteTool(sched *scheduler.Scheduler) *ScheduleDeleteTool {
	return &ScheduleDeleteTool{sched: sched}
}

func (t *ScheduleDeleteTool) Name() string { return "schedule_delete" }
func (t *ScheduleDeleteTool) Description() string {
	return "Cancel and delete a scheduled job by its ID (e.g. 'job-1729200000')."
}
func (t *ScheduleDeleteTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"id": {
				"type": "string",
				"description": "Job ID to delete."
			}
		},
		"required": ["id"]
	}`)
}

func (t *ScheduleDeleteTool) Execute(_ context.Context, rawArgs string) (string, error) {
	var args struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(rawArgs), &args); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	if t.sched.DeleteJob(strings.TrimSpace(args.ID)) {
		return fmt.Sprintf("Scheduled job %s deleted.", args.ID), nil
	}
	return fmt.Sprintf("Scheduled job %s not found.", args.ID), nil
}
