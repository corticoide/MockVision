package worker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/corticoide/mockvision/backend/internal/domain"
	"github.com/corticoide/mockvision/backend/internal/store"
	"github.com/corticoide/mockvision/backend/internal/store/db"
)

type pub struct {
	mu        sync.Mutex
	published map[string]int
	forgotten map[string]bool
}

func newPub() *pub { return &pub{published: map[string]int{}, forgotten: map[string]bool{}} }

func (p *pub) Publish(topic, typ string, data any) {
	p.mu.Lock()
	p.published[topic]++
	p.mu.Unlock()
}

func (p *pub) Forget(topic string) {
	p.mu.Lock()
	p.forgotten[topic] = true
	p.mu.Unlock()
}

type harness struct {
	t      *testing.T
	st     *store.Store
	pub    *pub
	r      *Runner
	cancel context.CancelFunc
	done   chan struct{}
	max    atomic.Int64
}

func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// newHarness builds a runner on st; start runs it.
func newHarness(t *testing.T, st *store.Store, kinds map[string]Kind) *harness {
	h := &harness{t: t, st: st, pub: newPub()}
	h.max.Store(2)
	h.r = New(Config{
		Store: st, Publisher: h.pub, MaxRunning: func() int { return int(h.max.Load()) },
		StepTimeout: func() time.Duration { return 200 * time.Millisecond },
		Log:         slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	for typ, k := range kinds {
		h.r.Register(typ, k)
	}
	return h
}

func (h *harness) start() {
	if err := h.r.Recover(context.Background()); err != nil {
		h.t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel, h.done = cancel, make(chan struct{})
	go func() {
		h.r.Run(ctx)
		close(h.done)
	}()
	h.t.Cleanup(h.stop)
}

// stop shuts the runner down, as when the node stops.
func (h *harness) stop() {
	if h.cancel == nil {
		return
	}
	h.cancel()
	<-h.done
	h.cancel = nil
}

func (h *harness) submit(spec Spec) Job {
	h.t.Helper()
	j, _, err := h.r.Submit(context.Background(), spec)
	if err != nil {
		h.t.Fatal(err)
	}
	return j
}

func (h *harness) wait(id string) Job {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	j, err := h.r.Wait(ctx, id)
	if err != nil {
		h.t.Fatalf("wait %s: %v (status %s)", id, err, j.Status)
	}
	return j
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (h *harness) status(id string) Status {
	j, err := h.r.Get(context.Background(), id)
	if err != nil {
		h.t.Fatal(err)
	}
	return j.Status
}

func TestQueueRespectsTheLimitAndOrder(t *testing.T) {
	var running, peak atomic.Int64
	var mu sync.Mutex
	var order []string
	release := make(chan struct{})
	h := newHarness(t, openStore(t), map[string]Kind{"block": {Handler: func(ctx context.Context, run *Run) (any, error) {
		n := running.Add(1)
		defer running.Add(-1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		var p struct{ N int }
		_ = run.Params(&p)
		mu.Lock()
		order = append(order, strings.Repeat("x", p.N))
		mu.Unlock()
		run.Step("working", 0.5)
		<-release
		return map[string]int{"n": p.N}, nil
	}}})
	h.max.Store(1)
	h.start()
	var jobs []Job
	for i := 1; i <= 3; i++ {
		jobs = append(jobs, h.submit(Spec{Type: "block", Title: "job", Params: map[string]int{"N": i}, CreatedBy: "test"}))
	}
	waitFor(t, "the first job to run", func() bool { return h.status(jobs[0].ID) == Running })
	time.Sleep(50 * time.Millisecond)
	if h.status(jobs[1].ID) != Queued || h.status(jobs[2].ID) != Queued {
		t.Fatal("jobs beyond the limit must wait in the queue")
	}
	// Raising the limit lets the next one start without a restart.
	h.max.Store(2)
	waitFor(t, "the second job to run", func() bool { return h.status(jobs[1].ID) == Running })
	// Its status is saved before its handler starts: wait for the handler
	// too, or under load the third job could get ahead of it.
	waitFor(t, "the second handler to start", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(order) == 2
	})
	close(release)
	for _, j := range jobs {
		got := h.wait(j.ID)
		if got.Status != Completed || got.Progress != 1 || got.FinishedAt == nil || string(got.Result) == "{}" {
			t.Fatalf("job %s: %+v", j.ID, got)
		}
	}
	if peak.Load() != 2 || strings.Join(order, ",") != "x,xx,xxx" {
		t.Fatalf("peak %d, order %v", peak.Load(), order)
	}
	events, err := h.r.Events(context.Background(), jobs[0].ID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for i, e := range events {
		if e.Seq != int64(i+1) {
			t.Fatalf("seq %d at %d", e.Seq, i)
		}
		kinds = append(kinds, e.Kind)
	}
	if strings.Join(kinds, ",") != "status,status,step,status" {
		t.Fatalf("events %v", kinds)
	}
	if !h.pub.forgotten["job:"+jobs[0].ID] {
		t.Fatal("a finished job's topic is freed")
	}
}

func TestSameKeyJoinsTheOpenJob(t *testing.T) {
	release := make(chan struct{})
	h := newHarness(t, openStore(t), map[string]Kind{"enc": {Handler: func(ctx context.Context, run *Run) (any, error) {
		<-release
		return nil, nil
	}}})
	h.start()
	a, created, err := h.r.Submit(context.Background(), Spec{Type: "enc", Key: "rendition:abc"})
	if err != nil || !created {
		t.Fatal(created, err)
	}
	b, created, err := h.r.Submit(context.Background(), Spec{Type: "enc", Key: "rendition:abc"})
	if err != nil || created || b.ID != a.ID {
		t.Fatalf("second submit: %v %v %v", b.ID, created, err)
	}
	close(release)
	h.wait(a.ID)
	c, created, _ := h.r.Submit(context.Background(), Spec{Type: "enc", Key: "rendition:abc"})
	if !created || c.ID == a.ID {
		t.Fatal("a finished job does not absorb new requests")
	}
	h.wait(c.ID)
	if _, _, err := h.r.Submit(context.Background(), Spec{Type: "nope"}); err == nil {
		t.Fatal("unknown type accepted")
	}
}

func TestCancel(t *testing.T) {
	started := make(chan struct{}, 1)
	var cleaned atomic.Int64
	h := newHarness(t, openStore(t), map[string]Kind{"long": {
		Handler: func(ctx context.Context, run *Run) (any, error) {
			started <- struct{}{}
			<-ctx.Done()
			return nil, ctx.Err()
		},
		Cleanup: func(Job) { cleaned.Add(1) },
	}})
	h.max.Store(1)
	h.start()
	running := h.submit(Spec{Type: "long"})
	queued := h.submit(Spec{Type: "long"})
	<-started
	j, err := h.r.Cancel(context.Background(), queued.ID)
	if err != nil || j.Status != Canceled {
		t.Fatalf("cancel queued: %+v %v", j, err)
	}
	if _, err := h.r.Cancel(context.Background(), running.ID); err != nil {
		t.Fatal(err)
	}
	got := h.wait(running.ID)
	if got.Status != Canceled || got.Error != "canceled by the user" {
		t.Fatalf("cancel running: %+v", got)
	}
	var conflict *domain.ConflictError
	if _, err := h.r.Cancel(context.Background(), running.ID); !errors.As(err, &conflict) {
		t.Fatalf("cancel a finished job: %v", err)
	}
	if _, err := h.r.Cancel(context.Background(), "missing"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("cancel unknown: %v", err)
	}
	waitFor(t, "both cleanups", func() bool { return cleaned.Load() == 2 })
}

// steps runs three steps and saves a checkpoint after each; blockAt makes
// it wait before a step until its context ends.
func steps(done *[]int, mu *sync.Mutex, blockAt int, reached chan<- struct{}) Handler {
	return func(ctx context.Context, run *Run) (any, error) {
		var cp struct{ Done int }
		run.Checkpoint(&cp)
		for i := cp.Done + 1; i <= 3; i++ {
			if i == blockAt {
				reached <- struct{}{}
				<-ctx.Done()
				return nil, ctx.Err()
			}
			run.Step("step", float64(i)/3)
			mu.Lock()
			*done = append(*done, i)
			mu.Unlock()
			cp.Done = i
			if err := run.Save(cp); err != nil {
				return nil, err
			}
		}
		return cp, nil
	}
}

func TestInterruptedJobResumesFromItsCheckpoint(t *testing.T) {
	st := openStore(t)
	var mu sync.Mutex
	var done []int
	reached := make(chan struct{}, 1)
	h := newHarness(t, st, map[string]Kind{"steps": {Handler: steps(&done, &mu, 3, reached)}})
	h.start()
	j := h.submit(Spec{Type: "steps", Key: "k"})
	<-reached
	h.stop() // the node stops while step 3 runs
	if got := h.status(j.ID); got != Interrupted {
		t.Fatalf("after the stop: %s", got)
	}

	// The node starts again: the job stays interrupted until resumed.
	h2 := newHarness(t, st, map[string]Kind{"steps": {Handler: steps(&done, &mu, 0, nil)}})
	h2.start()
	time.Sleep(50 * time.Millisecond)
	if got := h2.status(j.ID); got != Interrupted {
		t.Fatalf("after the restart: %s", got)
	}
	if _, err := h2.r.Resume(context.Background(), j.ID); err != nil {
		t.Fatal(err)
	}
	got := h2.wait(j.ID)
	if got.Status != Completed || string(got.Result) != `{"Done":3}` {
		t.Fatalf("resumed: %+v", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(done) != 3 || done[2] != 3 {
		t.Fatalf("steps run: %v (steps 1 and 2 must not repeat)", done)
	}
	var conflict *domain.ConflictError
	if _, err := h2.r.Resume(context.Background(), j.ID); !errors.As(err, &conflict) {
		t.Fatalf("resume a completed job: %v", err)
	}
}

func TestRecoverMarksCrashedJobsInterrupted(t *testing.T) {
	st := openStore(t)
	h := newHarness(t, st, map[string]Kind{"x": {Handler: func(ctx context.Context, run *Run) (any, error) { return nil, nil }}})
	j, _, _ := h.r.Submit(context.Background(), Spec{Type: "x", Key: "crash"})
	// A crash left the job running, waiting on a question.
	now := time.Now()
	running := j
	running.Status, running.StartedAt = Running, &now
	running.Question = &Question{ID: "q", Text: "?"}
	if err := st.W().UpdateJob(context.Background(), running.update()); err != nil {
		t.Fatal(err)
	}
	h.start()
	got, _ := h.r.Get(context.Background(), j.ID)
	if got.Status != Interrupted || got.Question != nil {
		t.Fatalf("after recover: %+v", got)
	}
	// Asking for the same work resumes it.
	again, created, err := h.r.Submit(context.Background(), Spec{Type: "x", Key: "crash"})
	if err != nil || created || again.ID != j.ID {
		t.Fatalf("submit the same key: %v %v %v", again.ID, created, err)
	}
	if got := h.wait(j.ID); got.Status != Completed {
		t.Fatalf("resumed by the new request: %s", got.Status)
	}
}

func TestQuestions(t *testing.T) {
	answers := make(chan string, 3)
	h := newHarness(t, openStore(t), map[string]Kind{"ask": {Handler: func(ctx context.Context, run *Run) (any, error) {
		opts := []Option{{ID: "skip", Label: "Skip"}, {ID: "retry", Label: "Retry"}}
		a, err := run.Ask(ctx, Question{Text: "A rendition failed", Options: opts, Default: "skip"}, 5*time.Second)
		if err != nil {
			return nil, err
		}
		answers <- a
		a, err = run.Ask(ctx, Question{Text: "Again?", Options: opts, Default: "retry"}, 20*time.Millisecond)
		if err != nil {
			return nil, err
		}
		answers <- a
		_, err = run.Ask(ctx, Question{Text: "No default", Options: opts}, 20*time.Millisecond)
		return nil, err
	}}})
	h.start()
	j := h.submit(Spec{Type: "ask"})
	waitFor(t, "the question", func() bool { return h.status(j.ID) == Waiting })
	got, _ := h.r.Get(context.Background(), j.ID)
	if got.Question == nil || got.Question.Text != "A rendition failed" || got.Question.ExpiresAt.IsZero() {
		t.Fatalf("question: %+v", got.Question)
	}
	if _, err := h.r.Answer(context.Background(), j.ID, "burn it"); err == nil {
		t.Fatal("an answer outside the options was accepted")
	}
	if _, err := h.r.Answer(context.Background(), j.ID, "retry"); err != nil {
		t.Fatal(err)
	}
	if a := <-answers; a != "retry" {
		t.Fatalf("first answer %s", a)
	}
	if a := <-answers; a != "retry" {
		t.Fatalf("expired question takes the default, got %s", a)
	}
	final := h.wait(j.ID)
	if final.Status != Canceled || !strings.Contains(final.Error, "nobody answered") {
		t.Fatalf("no default: %+v", final)
	}
	var conflict *domain.ConflictError
	if _, err := h.r.Answer(context.Background(), j.ID, "skip"); !errors.As(err, &conflict) {
		t.Fatalf("answer a finished job: %v", err)
	}
	events, _ := h.r.Events(context.Background(), j.ID, 0, 0)
	var byTimeout int
	for _, e := range events {
		if e.Kind == EventAnswer && strings.Contains(string(e.Data), `"by":"timeout"`) {
			byTimeout++
		}
	}
	if byTimeout != 2 {
		t.Fatalf("answers by timeout: %d", byTimeout)
	}
}

// A job waiting for an answer does not hold back the queue.
func TestWaitingHoldsNoSlot(t *testing.T) {
	ran := make(chan struct{})
	h := newHarness(t, openStore(t), map[string]Kind{
		"ask": {Handler: func(ctx context.Context, run *Run) (any, error) {
			return run.Ask(ctx, Question{Text: "Go on?", Options: []Option{{ID: "yes", Label: "Yes"}}, Default: "yes"}, time.Minute)
		}},
		"quick": {Handler: func(context.Context, *Run) (any, error) {
			close(ran)
			return nil, nil
		}},
	})
	h.max.Store(1)
	h.start()
	asking := h.submit(Spec{Type: "ask"})
	waitFor(t, "the question", func() bool { return h.status(asking.ID) == Waiting })
	quick := h.submit(Spec{Type: "quick"})
	select {
	case <-ran:
	case <-time.After(5 * time.Second):
		t.Fatalf("the queued job did not run while the other waited: %s", h.status(quick.ID))
	}
	if _, err := h.r.Answer(context.Background(), asking.ID, "yes"); err != nil {
		t.Fatal(err)
	}
	if j := h.wait(asking.ID); j.Status != Completed {
		t.Fatalf("after the answer: %+v", j)
	}
}

func TestStepTimeoutAndPanic(t *testing.T) {
	h := newHarness(t, openStore(t), map[string]Kind{
		"slow": {Handler: func(ctx context.Context, run *Run) (any, error) {
			sctx, cancel := run.StepContext(ctx)
			defer cancel()
			<-sctx.Done()
			return map[string]string{"partial": "yes"}, context.Cause(sctx)
		}},
		"boom": {Handler: func(ctx context.Context, run *Run) (any, error) { panic("kaboom") }},
	})
	h.start()
	slow := h.wait(h.submit(Spec{Type: "slow"}).ID)
	if slow.Status != Failed || !strings.Contains(slow.Error, "took longer than 200ms") || string(slow.Result) != `{"partial":"yes"}` {
		t.Fatalf("step timeout: %+v", slow)
	}
	boom := h.wait(h.submit(Spec{Type: "boom"}).ID)
	if boom.Status != Failed || !strings.Contains(boom.Error, "kaboom") {
		t.Fatalf("panic: %+v", boom)
	}
}

func TestListAndPrune(t *testing.T) {
	st := openStore(t)
	h := newHarness(t, st, map[string]Kind{"ok": {Handler: func(ctx context.Context, run *Run) (any, error) { return nil, nil }}})
	h.max.Store(0) // nothing runs
	h.start()
	a := h.submit(Spec{Type: "ok"})
	b := h.submit(Spec{Type: "ok"})
	active, _, err := h.r.List(context.Background(), Filter{Status: "active"})
	if err != nil || len(active) != 2 || active[0].ID != b.ID {
		t.Fatalf("active: %v %v", active, err)
	}
	h.max.Store(2)
	h.wait(a.ID)
	h.wait(b.ID)
	finished, _, _ := h.r.List(context.Background(), Filter{Status: "finished", Limit: 1})
	page, next, _ := h.r.List(context.Background(), Filter{Limit: 1})
	if len(finished) != 1 || len(page) != 1 || next == "" {
		t.Fatalf("pages: %d %d %q", len(finished), len(page), next)
	}
	if _, _, err := h.r.List(context.Background(), Filter{Status: "bogus"}); err == nil {
		t.Fatal("unknown status accepted")
	}
	n, err := h.r.Prune(context.Background(), time.Now().Add(time.Minute))
	if err != nil || n != 2 {
		t.Fatalf("prune: %d %v", n, err)
	}
	if _, err := h.r.Get(context.Background(), a.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("pruned job: %v", err)
	}
	if rows, _ := st.R().ListJobEvents(context.Background(), db.ListJobEventsParams{JobID: a.ID, Limit: 10}); len(rows) != 0 {
		t.Fatal("events outlive their job")
	}
}

// Anything that queues work without limit is refused once the queue is
// full; joining an open job still works.
func TestQueueIsBounded(t *testing.T) {
	h := newHarness(t, openStore(t), map[string]Kind{"noop": {Handler: func(context.Context, *Run) (any, error) { return nil, nil }}})
	for i := range MaxQueued {
		h.submit(Spec{Type: "noop", Key: fmt.Sprintf("k%d", i)})
	}
	_, _, err := h.r.Submit(context.Background(), Spec{Type: "noop", Key: "one-too-many"})
	var cerr *domain.ConflictError
	if !errors.As(err, &cerr) {
		t.Fatalf("want a conflict, got %v", err)
	}
	if _, created, err := h.r.Submit(context.Background(), Spec{Type: "noop", Key: "k7"}); err != nil || created {
		t.Fatalf("joining an open job: created %v, %v", created, err)
	}
}

// An interrupted job nobody resumes is canceled, and cleaned up, once it
// is older than the retention (audit B14).
func TestPruneCancelsStaleInterruptedJobs(t *testing.T) {
	st := openStore(t)
	var mu sync.Mutex
	var done []int
	reached := make(chan struct{}, 1)
	var cleaned atomic.Int32
	kind := Kind{Handler: steps(&done, &mu, 1, reached), Cleanup: func(Job) { cleaned.Add(1) }}
	h := newHarness(t, st, map[string]Kind{"steps": kind})
	h.start()
	j := h.submit(Spec{Type: "steps", Key: "stale"})
	<-reached
	h.stop()
	if got := h.status(j.ID); got != Interrupted {
		t.Fatalf("after the stop: %s", got)
	}
	h2 := newHarness(t, st, map[string]Kind{"steps": kind})
	if _, err := h2.r.Prune(context.Background(), time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got := h2.status(j.ID); got != Interrupted {
		t.Fatalf("a recent interrupted job must be kept: %s", got)
	}
	if _, err := h2.r.Prune(context.Background(), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "cleanup", func() bool { return cleaned.Load() == 1 })
	// Canceled past the retention, it is pruned in the same pass.
	if _, err := h2.r.Get(context.Background(), j.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("an old interrupted job must be gone: %v", err)
	}
}
