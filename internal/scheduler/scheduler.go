package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
)

// JobMode defines whether a scheduled job runs an autonomous agent task loop or sends a direct message.
type JobMode string

const (
	ModeAgentTask     JobMode = "agent_task"
	ModeDirectMessage JobMode = "direct_message"
)

// Job represents a persistent scheduled task or reminder.
type Job struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Spec       string    `json:"spec"` // e.g. "@every 30m", "@daily 09:00", "@once 10m", "*/15 * * * *"
	Mode       JobMode   `json:"mode"`
	Prompt     string    `json:"prompt"`
	Channel    string    `json:"channel"`
	Recipient  string    `json:"recipient"`
	OneShot    bool      `json:"one_shot"`
	NextRunAt  time.Time `json:"next_run_at"`
	LastRunAt  time.Time `json:"last_run_at,omitempty"`
	RunCount   int       `json:"run_count"`
	CreatedAt  time.Time `json:"created_at"`
}

// TriggerHandler is invoked when a scheduled job fires.
type TriggerHandler func(ctx context.Context, job Job)

// Scheduler manages persistent cron/interval/one-shot jobs and triggers the Agent in the background.
type Scheduler struct {
	mu          sync.RWMutex
	seq         int64
	storagePath string
	jobs        map[string]*Job
	onTrigger   TriggerHandler
}

func NewScheduler(stateDir string, onTrigger TriggerHandler) (*Scheduler, error) {
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return nil, fmt.Errorf("create state dir: %w", err)
	}
	s := &Scheduler{
		storagePath: filepath.Join(stateDir, "schedules.json"),
		jobs:        make(map[string]*Job),
		onTrigger:   onTrigger,
	}
	_ = s.load()
	return s, nil
}

func (s *Scheduler) SetTriggerHandler(fn TriggerHandler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onTrigger = fn
}

// Start runs the background scheduler loop checking due jobs every second.
func (s *Scheduler) Start(ctx context.Context) {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	log.Info().Int("loaded_jobs", s.Count()).Msg("Hermes-style autonomous scheduler started")

	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			s.tick(ctx, now)
		}
	}
}

func (s *Scheduler) tick(ctx context.Context, now time.Time) {
	var dueJobs []Job

	s.mu.Lock()
	dirty := false
	for id, job := range s.jobs {
		if ! now.Before(job.NextRunAt) {
			job.LastRunAt = now
			job.RunCount++
			dueJobs = append(dueJobs, *job)

			if job.OneShot {
				delete(s.jobs, id)
				dirty = true
			} else {
				next, _, err := computeNextRun(job.Spec, now)
				if err != nil {
					log.Warn().Err(err).Str("job_id", id).Msg("Disabling job with invalid next schedule")
					delete(s.jobs, id)
				} else {
					job.NextRunAt = next
				}
				dirty = true
			}
		}
	}
	if dirty {
		_ = s.saveLocked()
	}
	handler := s.onTrigger
	s.mu.Unlock()

	if handler == nil {
		return
	}
	for _, j := range dueJobs {
		jobCopy := j
		go func() {
			log.Info().
				Str("job_id", jobCopy.ID).
				Str("name", jobCopy.Name).
				Str("mode", string(jobCopy.Mode)).
				Msg("Executing scheduled job")
			handler(ctx, jobCopy)
		}()
	}
}

// AddJob validates the schedule spec, computes NextRunAt, persists the job, and returns it.
func (s *Scheduler) AddJob(name, spec string, mode JobMode, prompt, channelName, recipient string) (*Job, error) {
	spec = strings.TrimSpace(spec)
	nextRun, oneShot, err := computeNextRun(spec, time.Now())
	if err != nil {
		return nil, err
	}
	if mode != ModeDirectMessage {
		mode = ModeAgentTask
	}
	if strings.TrimSpace(name) == "" {
		name = "Scheduled Task"
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++

	job := &Job{
		ID:        fmt.Sprintf("job-%d-%d", time.Now().Unix(), s.seq),
		Name:      strings.TrimSpace(name),
		Spec:      spec,
		Mode:      mode,
		Prompt:    strings.TrimSpace(prompt),
		Channel:   channelName,
		Recipient: recipient,
		OneShot:   oneShot,
		NextRunAt: nextRun,
		CreatedAt: time.Now(),
	}
	s.jobs[job.ID] = job
	if err := s.saveLocked(); err != nil {
		return nil, err
	}
	return job, nil
}

// ListJobs returns all active scheduled jobs ordered by NextRunAt.
func (s *Scheduler) ListJobs() []Job {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]Job, 0, len(s.jobs))
	for _, j := range s.jobs {
		out = append(out, *j)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].NextRunAt.Before(out[j].NextRunAt)
	})
	return out
}

// DeleteJob removes a scheduled job by ID.
func (s *Scheduler) DeleteJob(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.jobs[id]; !ok {
		return false
	}
	delete(s.jobs, id)
	_ = s.saveLocked()
	return true
}

func (s *Scheduler) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.jobs)
}

func (s *Scheduler) load() error {
	data, err := os.ReadFile(s.storagePath)
	if err != nil {
		return err
	}
	var list []*Job
	if err := json.Unmarshal(data, &list); err != nil {
		return err
	}
	now := time.Now()
	for _, j := range list {
		if j.OneShot && j.NextRunAt.Before(now) {
			// Run missed one-shot job 5 seconds after startup
			j.NextRunAt = now.Add(5 * time.Second)
		} else if !j.OneShot && j.NextRunAt.Before(now) {
			if next, _, err := computeNextRun(j.Spec, now); err == nil {
				j.NextRunAt = next
			}
		}
		s.jobs[j.ID] = j
	}
	return nil
}

func (s *Scheduler) saveLocked() error {
	list := make([]*Job, 0, len(s.jobs))
	for _, j := range s.jobs {
		list = append(list, j)
	}
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.storagePath, data, 0o644)
}

// computeNextRun supports:
// - "@once 10m" / "in 10m" / "@once 2026-10-18 09:00:00"
// - "@every 30m" / "30m"
// - "@daily 09:00" / "09:00"
// - 5-field cron: "*/5 * * * *", "0 9 * * *"
func computeNextRun(spec string, from time.Time) (time.Time, bool, error) {
	s := strings.TrimSpace(spec)
	lower := strings.ToLower(s)

	// 1. One-shot timer: "@once 10m", "in 5m", or "@once 2026-10-18 12:00:00"
	if strings.HasPrefix(lower, "@once ") || strings.HasPrefix(lower, "in ") || strings.HasPrefix(lower, "once ") {
		arg := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(strings.TrimPrefix(lower, "@once "), "once "), "in "))
		if d, err := time.ParseDuration(arg); err == nil && d > 0 {
			return from.Add(d), true, nil
		}
		if t, err := time.ParseInLocation("2006-01-02 15:04:05", arg, from.Location()); err == nil {
			return t, true, nil
		}
		if t, err := time.ParseInLocation("2006-01-02 15:04", arg, from.Location()); err == nil {
			return t, true, nil
		}
		return time.Time{}, false, fmt.Errorf("invalid @once spec %q (use e.g. '@once 10m' or '@once 2026-10-18 09:00')", spec)
	}

	// 2. Recurring interval: "@every 30m" or plain duration "30m"
	if strings.HasPrefix(lower, "@every ") || strings.HasPrefix(lower, "every ") {
		arg := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(lower, "@every "), "every "))
		d, err := time.ParseDuration(arg)
		if err != nil || d < 10*time.Second {
			return time.Time{}, false, fmt.Errorf("invalid @every duration %q (minimum 10s, e.g. '@every 5m')", spec)
		}
		return from.Add(d), false, nil
	}
	if d, err := time.ParseDuration(lower); err == nil && d >= 10*time.Second {
		return from.Add(d), false, nil
	}

	// 3. Daily at HH:MM: "@daily 09:00" or "09:00"
	dailyArg := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(lower, "@daily"), "daily"))
	if parts := strings.Split(dailyArg, ":"); len(parts) == 2 && len(dailyArg) <= 5 {
		hour, errH := strconv.Atoi(parts[0])
		min, errM := strconv.Atoi(parts[1])
		if errH == nil && errM == nil && hour >= 0 && hour <= 23 && min >= 0 && min <= 59 {
			next := time.Date(from.Year(), from.Month(), from.Day(), hour, min, 0, 0, from.Location())
			if !next.After(from) {
				next = next.Add(24 * time.Hour)
			}
			return next, false, nil
		}
	}

	// 4. Standard 5-field cron: "min hour dom month dow"
	fields := strings.Fields(s)
	if len(fields) == 5 {
		next, err := nextCronTime(fields, from)
		if err != nil {
			return time.Time{}, false, err
		}
		return next, false, nil
	}

	return time.Time{}, false, fmt.Errorf("unsupported schedule format %q (supported: '@once 10m', '@every 30m', '@daily 09:00', or 5-field cron '*/15 * * * *')", spec)
}

func nextCronTime(fields []string, from time.Time) (time.Time, error) {
	// Search minute-by-minute up to 366 days ahead
	candidate := from.Truncate(time.Minute).Add(time.Minute)
	limit := candidate.Add(366 * 24 * time.Hour)

	for candidate.Before(limit) {
		if matchCronField(fields[0], candidate.Minute(), 0, 59) &&
			matchCronField(fields[1], candidate.Hour(), 0, 23) &&
			matchCronField(fields[2], candidate.Day(), 1, 31) &&
			matchCronField(fields[3], int(candidate.Month()), 1, 12) &&
			matchCronField(fields[4], int(candidate.Weekday()), 0, 6) {
			return candidate, nil
		}
		candidate = candidate.Add(time.Minute)
	}
	return time.Time{}, fmt.Errorf("cron expression %q did not match any time within 1 year", strings.Join(fields, " "))
}

func matchCronField(expr string, val, minVal, maxVal int) bool {
	expr = strings.TrimSpace(expr)
	if expr == "*" {
		return true
	}
	for _, part := range strings.Split(expr, ",") {
		part = strings.TrimSpace(part)
		if strings.HasPrefix(part, "*/") {
			step, err := strconv.Atoi(strings.TrimPrefix(part, "*/"))
			if err == nil && step > 0 && (val-minVal)%step == 0 {
				return true
			}
			continue
		}
		if n, err := strconv.Atoi(part); err == nil && n >= minVal && n <= maxVal && n == val {
			return true
		}
	}
	return false
}
