package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/oklog/ulid/v2"
)

// progressEvery bounds how often progress is written: the database may
// live on an SD card.
const progressEvery = time.Second

// Run is what a handler uses to report on its job and keep it resumable.
type Run struct {
	r  *Runner
	ex *execution
}

// ID of the job.
func (run *Run) ID() string { return run.ex.job.ID }

// CreatedBy is who submitted the job.
func (run *Run) CreatedBy() string { return run.ex.job.CreatedBy }

// Params decodes the job's parameters.
func (run *Run) Params(v any) error {
	return json.Unmarshal(run.ex.job.params, v)
}

// Checkpoint decodes the last saved checkpoint; it reports false when the
// job starts from the beginning.
func (run *Run) Checkpoint(v any) bool {
	cp := run.ex.job.checkpoint
	if len(cp) == 0 || string(cp) == "{}" || string(cp) == "null" {
		return false
	}
	return json.Unmarshal(cp, v) == nil
}

// Save stores a checkpoint: after a restart the job resumes from it.
func (run *Run) Save(v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	r := run.r
	r.mu.Lock()
	defer r.mu.Unlock()
	run.ex.job.checkpoint = raw
	return r.save(context.Background(), &run.ex.job)
}

// Step starts a named step at the given overall progress (0 to 1).
func (run *Run) Step(name string, progress float64) {
	r := run.r
	r.mu.Lock()
	defer r.mu.Unlock()
	run.ex.job.Step, run.ex.job.Progress = name, clamp(progress)
	run.ex.lastSave = time.Now()
	_ = r.save(context.Background(), &run.ex.job)
	r.event(context.Background(), run.ex.job.ID, EventStep, map[string]any{"step": name, "progress": run.ex.job.Progress})
}

// Progress sets the overall progress (0 to 1). It is shown live and
// written at most once a second.
func (run *Run) Progress(progress float64) {
	r := run.r
	r.mu.Lock()
	defer r.mu.Unlock()
	p := clamp(progress)
	if p < run.ex.job.Progress {
		return
	}
	run.ex.job.Progress = p
	if time.Since(run.ex.lastSave) < progressEvery {
		r.cfg.Publisher.Publish("jobs", "job", run.ex.job)
		return
	}
	run.ex.lastSave = time.Now()
	_ = r.save(context.Background(), &run.ex.job)
}

// Logf adds a line to the job's history.
func (run *Run) Logf(format string, args ...any) {
	r := run.r
	r.mu.Lock()
	defer r.mu.Unlock()
	r.event(context.Background(), run.ex.job.ID, EventLog, map[string]string{"message": fmt.Sprintf(format, args...)})
}

// StepContext bounds one step by the configured step timeout (D72).
func (run *Run) StepContext(ctx context.Context) (context.Context, context.CancelFunc) {
	d := run.r.cfg.StepTimeout()
	return context.WithTimeoutCause(ctx, d, fmt.Errorf("the step took longer than %s", d))
}

// Ask puts the job in Waiting until the user answers the question or it
// expires after timeout. On expiry the job goes on with the default
// answer, or fails with ErrUnanswered when there is none. While it waits
// the job holds no slot, so after the answer more jobs than the limit may
// run for a while.
func (run *Run) Ask(ctx context.Context, q Question, timeout time.Duration) (string, error) {
	r := run.r
	q.ID = ulid.Make().String()
	q.ExpiresAt = time.Now().Add(timeout).UTC()
	r.mu.Lock()
	// An answer left over from an earlier question does not count.
	select {
	case <-run.ex.answer:
	default:
	}
	run.ex.job.Status, run.ex.job.Question = Waiting, &q
	_ = r.save(context.Background(), &run.ex.job)
	r.event(context.Background(), run.ex.job.ID, EventQuestion, q)
	r.mu.Unlock()
	r.wake() // its slot is free while it waits

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	var answer, by string
	select {
	case answer = <-run.ex.answer:
		by = "user"
	case <-timer.C:
		answer, by = q.Default, "timeout"
	case <-ctx.Done():
		return "", context.Cause(ctx)
	}

	r.mu.Lock()
	run.ex.job.Status, run.ex.job.Question = Running, nil
	_ = r.save(context.Background(), &run.ex.job)
	r.event(context.Background(), run.ex.job.ID, EventAnswer, map[string]string{"question": q.ID, "answer": answer, "by": by})
	r.mu.Unlock()
	if answer == "" {
		return "", ErrUnanswered
	}
	return answer, nil
}

func clamp(p float64) float64 {
	switch {
	case p < 0:
		return 0
	case p > 1:
		return 1
	}
	return p
}
