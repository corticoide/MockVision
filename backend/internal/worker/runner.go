package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"slices"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/corticoide/mockvision/backend/internal/domain"
	"github.com/corticoide/mockvision/backend/internal/store"
	"github.com/corticoide/mockvision/backend/internal/store/db"
)

// Handler runs a job. It returns the job's result; with an error the job
// fails, and a result returned beside the error is kept as its detail.
type Handler func(ctx context.Context, run *Run) (any, error)

// Kind is a type of job.
type Kind struct {
	Handler Handler
	// Cleanup runs once a job of this kind is over for good (completed,
	// failed or canceled), to remove what it kept for resuming.
	Cleanup func(Job)
}

// Publisher sends live updates to the panel (the jobs and job:<id>
// topics of the WebSocket).
type Publisher interface {
	Publish(topic, typ string, data any)
	Forget(topic string)
}

// Config configures a runner. MaxRunning and StepTimeout are read every
// time they are needed, so settings apply without a restart.
type Config struct {
	Store       *store.Store
	Publisher   Publisher
	MaxRunning  func() int
	StepTimeout func() time.Duration
	Log         *slog.Logger
}

// MaxQueued bounds the jobs waiting in the queue. Anything that can queue
// work without limit, such as clients of a camera's emulated API changing
// its encoder settings, would otherwise grow it forever (audit A2).
const MaxQueued = 500

// ErrUnanswered ends a job whose question expired with no default.
var ErrUnanswered = errors.New("nobody answered the job's question in time")

// ErrStopped ends a job the user chose to stop through a question.
var ErrStopped = errors.New("stopped at the user's request")

var (
	errCanceled = errors.New("canceled by the user")
	errShutdown = errors.New("the node stopped")
)

// Runner queues and runs jobs.
type Runner struct {
	cfg   Config
	kinds map[string]Kind
	kick  chan struct{}

	mu       sync.Mutex
	running  map[string]*execution
	waiters  map[string][]chan struct{}
	seqs     map[string]int64
	stopping bool
	wg       sync.WaitGroup
}

type execution struct {
	job      Job
	cancel   context.CancelCauseFunc
	answer   chan string
	lastSave time.Time
}

// New builds a runner; register the kinds, then call Recover and Run.
func New(cfg Config) *Runner {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.MaxRunning == nil {
		cfg.MaxRunning = func() int { return 2 }
	}
	if cfg.StepTimeout == nil {
		cfg.StepTimeout = func() time.Duration { return 10 * time.Minute }
	}
	return &Runner{
		cfg: cfg, kinds: map[string]Kind{}, kick: make(chan struct{}, 1),
		running: map[string]*execution{}, waiters: map[string][]chan struct{}{}, seqs: map[string]int64{},
	}
}

// Register adds a kind of job.
func (r *Runner) Register(typ string, k Kind) { r.kinds[typ] = k }

// Known reports whether a type of job is registered.
func (r *Runner) Known(typ string) bool {
	_, ok := r.kinds[typ]
	return ok
}

func (r *Runner) wake() {
	select {
	case r.kick <- struct{}{}:
	default:
	}
}

// Recover marks the jobs that were running or waiting when the node
// stopped as Interrupted (D74). Queued jobs stay in the queue.
func (r *Runner) Recover(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, st := range []Status{Running, Waiting} {
		rows, err := r.cfg.Store.R().ListJobsByStatus(ctx, db.ListJobsByStatusParams{Status: string(st), Limit: 100_000})
		if err != nil {
			return err
		}
		for _, row := range rows {
			j := jobFrom(row)
			j.Status, j.Question = Interrupted, nil
			if err := r.save(ctx, &j); err != nil {
				return err
			}
			r.event(ctx, j.ID, EventStatus, map[string]string{"status": string(Interrupted), "reason": "the node stopped while the job ran"})
			r.cfg.Log.Info("job interrupted by the restart", "job", j.ID, "type", j.Type)
		}
	}
	return nil
}

// Run dispatches queued jobs until ctx is done, then interrupts the running
// ones and waits for them.
func (r *Runner) Run(ctx context.Context) {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		r.dispatch(ctx)
		select {
		case <-ctx.Done():
			r.mu.Lock()
			r.stopping = true
			for _, ex := range r.running {
				ex.cancel(errShutdown)
			}
			r.mu.Unlock()
			r.wg.Wait()
			return
		case <-r.kick:
		case <-tick.C: // picks up a larger MaxRunning
		}
	}
}

func (r *Runner) dispatch(ctx context.Context) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopping {
		return
	}
	// A job waiting for an answer holds no slot: a question left open
	// must not hold back the encodings cameras wait for.
	busy := 0
	for _, ex := range r.running {
		if ex.job.Status != Waiting {
			busy++
		}
	}
	free := r.cfg.MaxRunning() - busy
	if free <= 0 {
		return
	}
	rows, err := r.cfg.Store.R().ListJobsByStatus(ctx, db.ListJobsByStatusParams{Status: string(Queued), Limit: int64(free)})
	if err != nil {
		r.cfg.Log.Warn("cannot read the job queue", "error", err)
		return
	}
	for _, row := range rows {
		j := jobFrom(row)
		kind, ok := r.kinds[j.Type]
		if !ok {
			j.Status, j.Error = Failed, fmt.Sprintf("unknown job type %q", j.Type)
			now := time.Now()
			j.FinishedAt = &now
			_ = r.save(ctx, &j)
			r.event(ctx, j.ID, EventStatus, map[string]string{"status": string(Failed), "error": j.Error})
			continue
		}
		now := time.Now()
		j.Status, j.Error = Running, ""
		if j.StartedAt == nil {
			j.StartedAt = &now
		}
		if err := r.save(ctx, &j); err != nil {
			r.cfg.Log.Warn("cannot start job", "job", j.ID, "error", err)
			continue
		}
		r.event(ctx, j.ID, EventStatus, map[string]string{"status": string(Running)})
		jctx, cancel := context.WithCancelCause(context.WithoutCancel(ctx))
		ex := &execution{job: j, cancel: cancel, answer: make(chan string, 1)}
		r.running[j.ID] = ex
		r.wg.Add(1)
		go r.execute(jctx, ex, kind)
	}
}

func (r *Runner) execute(ctx context.Context, ex *execution, kind Kind) {
	defer r.wg.Done()
	run := &Run{r: r, ex: ex}
	result, err := call(ctx, kind.Handler, run)
	cause := context.Cause(ctx)
	ex.cancel(nil)

	bg := context.Background()
	r.mu.Lock()
	j := ex.job
	j.Question = nil
	if result != nil {
		if raw, merr := json.Marshal(result); merr == nil {
			j.Result = raw
		}
	}
	now := time.Now()
	switch {
	case err == nil:
		j.Status, j.Progress, j.Error = Completed, 1, ""
	case errors.Is(cause, errShutdown):
		j.Status, j.Error = Interrupted, ""
	case errors.Is(cause, errCanceled):
		j.Status, j.Error = Canceled, "canceled by the user"
	case errors.Is(err, ErrUnanswered), errors.Is(err, ErrStopped):
		j.Status, j.Error = Canceled, err.Error()
	default:
		j.Status, j.Error = Failed, err.Error()
	}
	if j.Status.Final() {
		j.FinishedAt = &now
	}
	if serr := r.save(bg, &j); serr != nil {
		r.cfg.Log.Warn("cannot save the end of a job", "job", j.ID, "error", serr)
	}
	data := map[string]string{"status": string(j.Status)}
	if j.Error != "" {
		data["error"] = j.Error
	}
	r.event(bg, j.ID, EventStatus, data)
	delete(r.running, j.ID)
	r.finishLocked(j)
	r.mu.Unlock()

	if j.Status.Final() && kind.Cleanup != nil {
		kind.Cleanup(j)
	}
	switch j.Status {
	case Failed:
		r.cfg.Log.Warn("job failed", "job", j.ID, "type", j.Type, "error", j.Error)
	case Completed:
		r.cfg.Log.Info("job completed", "job", j.ID, "type", j.Type, "took", now.Sub(*j.StartedAt).Round(time.Millisecond))
	}
	r.wake()
}

// call runs a handler, turning a panic into an error.
func call(ctx context.Context, h Handler, run *Run) (res any, err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("the job crashed: %v", p)
			run.r.cfg.Log.Error("job panicked", "job", run.ex.job.ID, "panic", p, "stack", string(debug.Stack()))
		}
	}()
	return h(ctx, run)
}

// finishLocked wakes whoever waits for a job that stopped running, and
// frees its live topic once it is over.
func (r *Runner) finishLocked(j Job) {
	if j.Status.Busy() {
		return
	}
	for _, ch := range r.waiters[j.ID] {
		close(ch)
	}
	delete(r.waiters, j.ID)
	if j.Status.Final() {
		delete(r.seqs, j.ID)
		r.cfg.Publisher.Forget("job:" + j.ID)
	}
}

// Spec describes a job to submit. Jobs with the same Key share one open
// job: submitting again joins it, and resumes it if it was interrupted.
type Spec struct {
	Type      string
	Title     string
	Params    any
	Key       string
	CreatedBy string
}

// Submit queues a job. It returns the job and whether it is a new one.
func (r *Runner) Submit(ctx context.Context, spec Spec) (Job, bool, error) {
	if _, ok := r.kinds[spec.Type]; !ok {
		return Job{}, false, domain.Invalid("type", "unknown job type %q", spec.Type)
	}
	params := []byte("{}")
	if spec.Params != nil {
		var err error
		if params, err = json.Marshal(spec.Params); err != nil {
			return Job{}, false, err
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if spec.Key != "" {
		row, err := r.cfg.Store.R().GetOpenJobByKey(ctx, store.NullString(spec.Key))
		if err == nil {
			j := jobFrom(row)
			if j.Status == Interrupted {
				if err := r.requeueLocked(ctx, &j); err != nil {
					return Job{}, false, err
				}
			}
			return j, false, nil
		}
		if !errors.Is(store.NotFound(err), domain.ErrNotFound) {
			return Job{}, false, err
		}
	}
	if n, err := r.cfg.Store.R().CountQueuedJobs(ctx); err != nil {
		return Job{}, false, err
	} else if n >= MaxQueued {
		return Job{}, false, domain.Conflict("", "the job queue is full (%d jobs waiting); try again when some have run", n)
	}
	now := time.Now()
	row := db.InsertJobParams{
		ID: ulid.Make().String(), Type: spec.Type, Title: spec.Title, ParamsJson: string(params),
		DedupeKey: store.NullString(spec.Key), CreatedBy: spec.CreatedBy, CreatedAt: now.UnixMilli(),
	}
	if err := r.cfg.Store.W().InsertJob(ctx, row); err != nil {
		return Job{}, false, err
	}
	j := Job{
		ID: row.ID, Type: spec.Type, Title: spec.Title, Status: Queued, Result: json.RawMessage("{}"),
		CreatedBy: spec.CreatedBy, CreatedAt: store.Time(row.CreatedAt), params: params, checkpoint: json.RawMessage("{}"),
	}
	r.event(ctx, j.ID, EventStatus, map[string]string{"status": string(Queued)})
	r.cfg.Publisher.Publish("jobs", "job", j)
	r.wake()
	return j, true, nil
}

func (r *Runner) requeueLocked(ctx context.Context, j *Job) error {
	j.Status, j.Error = Queued, ""
	if err := r.save(ctx, j); err != nil {
		return err
	}
	r.event(ctx, j.ID, EventStatus, map[string]string{"status": string(Queued), "reason": "resumed"})
	r.wake()
	return nil
}

// Get returns a job; a running one as it stands in memory.
func (r *Runner) Get(ctx context.Context, id string) (Job, error) {
	r.mu.Lock()
	if ex, ok := r.running[id]; ok {
		j := ex.job
		r.mu.Unlock()
		return j, nil
	}
	r.mu.Unlock()
	row, err := r.cfg.Store.R().GetJob(ctx, id)
	if err != nil {
		return Job{}, store.NotFound(err)
	}
	return jobFrom(row), nil
}

// Filter selects jobs. Status is a status, "active" (queued, running,
// waiting or interrupted) or "finished".
type Filter struct {
	Status string
	Type   string
	Cursor string
	Limit  int
}

// List returns jobs, newest first, and the cursor of the next page.
func (r *Runner) List(ctx context.Context, f Filter) ([]Job, string, error) {
	switch Status(f.Status) {
	case "", "active", "finished", Queued, Running, Waiting, Completed, Failed, Canceled, Interrupted:
	default:
		return nil, "", domain.Invalid("status", "unknown status %q", f.Status)
	}
	if f.Limit <= 0 || f.Limit > 200 {
		f.Limit = 50
	}
	rows, err := r.cfg.Store.R().ListJobs(ctx, db.ListJobsParams{Cursor: f.Cursor, Type: f.Type, Status: f.Status, Limit: int64(f.Limit + 1)})
	if err != nil {
		return nil, "", err
	}
	next := ""
	if len(rows) > f.Limit {
		rows = rows[:f.Limit]
		next = rows[len(rows)-1].ID
	}
	out := make([]Job, len(rows))
	r.mu.Lock()
	for i, row := range rows {
		if ex, ok := r.running[row.ID]; ok {
			out[i] = ex.job // fresher progress than the throttled row
		} else {
			out[i] = jobFrom(row)
		}
	}
	r.mu.Unlock()
	return out, next, nil
}

// Events returns a job's history after seq, oldest first.
func (r *Runner) Events(ctx context.Context, id string, after int64, limit int) ([]Event, error) {
	if limit <= 0 || limit > 1000 {
		limit = 500
	}
	rows, err := r.cfg.Store.R().ListJobEvents(ctx, db.ListJobEventsParams{JobID: id, After: after, Limit: int64(limit)})
	if err != nil {
		return nil, err
	}
	out := make([]Event, len(rows))
	for i, row := range rows {
		out[i] = eventFrom(row)
	}
	return out, nil
}

// Wait blocks until the job is no longer queued, running or waiting, and
// returns it.
func (r *Runner) Wait(ctx context.Context, id string) (Job, error) {
	ch := make(chan struct{})
	r.mu.Lock()
	r.waiters[id] = append(r.waiters[id], ch)
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.waiters[id] = slices.DeleteFunc(r.waiters[id], func(c chan struct{}) bool { return c == ch })
		if len(r.waiters[id]) == 0 {
			delete(r.waiters, id)
		}
		r.mu.Unlock()
	}()
	j, err := r.Get(ctx, id)
	if err != nil {
		return Job{}, err
	}
	if !j.Status.Busy() {
		return j, nil
	}
	select {
	case <-ch:
		return r.Get(context.WithoutCancel(ctx), id)
	case <-ctx.Done():
		return j, ctx.Err()
	}
}

// Cancel stops a job. A queued or interrupted job ends at once; a running
// one ends when its handler returns, which the returned job may not show
// yet.
func (r *Runner) Cancel(ctx context.Context, id string) (Job, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if ex, ok := r.running[id]; ok {
		ex.cancel(errCanceled)
		return ex.job, nil
	}
	row, err := r.cfg.Store.R().GetJob(ctx, id)
	if err != nil {
		return Job{}, store.NotFound(err)
	}
	j := jobFrom(row)
	if j.Status != Queued && j.Status != Interrupted {
		return j, domain.Conflict("", "the job is %s and cannot be canceled", j.Status)
	}
	now := time.Now()
	j.Status, j.Error, j.FinishedAt, j.Question = Canceled, "canceled by the user", &now, nil
	if err := r.save(ctx, &j); err != nil {
		return j, err
	}
	r.event(ctx, j.ID, EventStatus, map[string]string{"status": string(Canceled), "error": j.Error})
	r.finishLocked(j)
	if k, ok := r.kinds[j.Type]; ok && k.Cleanup != nil {
		go k.Cleanup(j)
	}
	return j, nil
}

// Resume puts an interrupted job back in the queue; it continues from its
// last checkpoint.
func (r *Runner) Resume(ctx context.Context, id string) (Job, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	row, err := r.cfg.Store.R().GetJob(ctx, id)
	if err != nil {
		return Job{}, store.NotFound(err)
	}
	j := jobFrom(row)
	if j.Status != Interrupted {
		return j, domain.Conflict("", "only an interrupted job can be resumed; this one is %s", j.Status)
	}
	if !r.Known(j.Type) {
		return j, domain.Conflict("", "unknown job type %q", j.Type)
	}
	return j, r.requeueLocked(ctx, &j)
}

// Answer answers the question a job is waiting on.
func (r *Runner) Answer(ctx context.Context, id, answer string) (Job, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	ex, ok := r.running[id]
	if !ok || ex.job.Status != Waiting || ex.job.Question == nil {
		if _, err := r.cfg.Store.R().GetJob(ctx, id); err != nil {
			return Job{}, store.NotFound(err)
		}
		return Job{}, domain.Conflict("", "the job is not waiting for an answer")
	}
	q := ex.job.Question
	if !slices.ContainsFunc(q.Options, func(o Option) bool { return o.ID == answer }) {
		return ex.job, domain.Invalid("answer", "must be one of the question's options")
	}
	select {
	case ex.answer <- answer:
	default:
		return ex.job, domain.Conflict("", "the question was already answered")
	}
	return ex.job, nil
}

// Prune deletes jobs finished before t, with their events.
func (r *Runner) Prune(ctx context.Context, before time.Time) (int, error) {
	ids, err := r.cfg.Store.R().ListFinishedJobsBefore(ctx, store.NullMillis(before))
	if err != nil {
		return 0, err
	}
	for _, id := range ids {
		if err := r.cfg.Store.W().DeleteJob(ctx, id); err != nil {
			return 0, err
		}
	}
	return len(ids), nil
}

// save persists a job and shows it live; the caller holds r.mu.
func (r *Runner) save(ctx context.Context, j *Job) error {
	if err := r.cfg.Store.W().UpdateJob(ctx, j.update()); err != nil {
		return err
	}
	r.cfg.Publisher.Publish("jobs", "job", *j)
	return nil
}

// event appends to a job's history; the caller holds r.mu.
func (r *Runner) event(ctx context.Context, id, kind string, data any) {
	seq, ok := r.seqs[id]
	if !ok {
		max, err := r.cfg.Store.R().MaxJobEventSeq(ctx, id)
		if err != nil {
			r.cfg.Log.Warn("cannot read job events", "job", id, "error", err)
			return
		}
		seq = max
	}
	seq++
	r.seqs[id] = seq
	raw, _ := json.Marshal(data)
	now := time.Now()
	err := r.cfg.Store.W().InsertJobEvent(ctx, db.InsertJobEventParams{JobID: id, Seq: seq, At: now.UnixMilli(), Kind: kind, PayloadJson: string(raw)})
	if err != nil {
		r.cfg.Log.Warn("cannot record job event", "job", id, "kind", kind, "error", err)
		return
	}
	r.cfg.Publisher.Publish("job:"+id, "event", Event{JobID: id, Seq: seq, At: store.Time(now.UnixMilli()), Kind: kind, Data: raw})
}
