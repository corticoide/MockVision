package delivery_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/corticoide/mockvision/backend/internal/engines/delivery"
	"github.com/corticoide/mockvision/backend/internal/engines/enginetest"
	"github.com/corticoide/mockvision/sdk/engine"
)

func dispatch(policy engine.DeliveryPolicy, t engine.Target) engine.Dispatch {
	return engine.Dispatch{Event: engine.Event{ID: "E1"}, Policy: policy, Targets: []engine.Target{t}}
}

func TestRunnerRetriesWithThePolicy(t *testing.T) {
	h := enginetest.NewHost(t)
	r := delivery.NewRunner(h, 2)
	defer r.Stop(context.Background())
	var calls atomic.Int32
	target := engine.Target{ID: "T1"}
	r.Go(dispatch(engine.DeliveryPolicy{Retries: 3, Backoff: 10 * time.Millisecond}, target), target, func(_ context.Context, attempt int) delivery.Attempt {
		calls.Add(1)
		if attempt < 3 {
			return delivery.Attempt{Status: 503, Err: errors.New("target answered 503")}
		}
		return delivery.Attempt{Status: 200, Bytes: 10}
	})
	reps := h.WaitReports(3, 5*time.Second)
	want := []string{engine.DeliveryRetry, engine.DeliveryRetry, engine.DeliveryOK}
	for i, w := range want {
		if reps[i].Status != w || reps[i].Attempt != i+1 || reps[i].TargetID != "T1" || reps[i].EventID != "E1" {
			t.Fatalf("report %d = %+v, want %s", i, reps[i], w)
		}
	}
	if reps[0].HTTPStatus != 503 || reps[2].HTTPStatus != 200 || calls.Load() != 3 {
		t.Fatalf("reports %+v after %d calls", reps, calls.Load())
	}
	if h := r.Health(engine.HealthOK); h.Requests != 1 || h.Errors != 2 || h.BytesOut != 10 {
		t.Errorf("health %+v", h)
	}
}

func TestRunnerTimesOutAndTargetOverridesThePolicy(t *testing.T) {
	h := enginetest.NewHost(t)
	r := delivery.NewRunner(h, 2)
	defer r.Stop(context.Background())
	retries, timeout := 0, 50*time.Millisecond
	// The profile retries three times; the target asks for none and a
	// shorter timeout.
	target := engine.Target{ID: "T1", Delivery: &engine.DeliveryOverride{Retries: &retries, Timeout: &timeout}}
	r.Go(dispatch(engine.DeliveryPolicy{Timeout: 5 * time.Second, Retries: 3}, target), target, func(ctx context.Context, _ int) delivery.Attempt {
		<-ctx.Done()
		return delivery.Attempt{Err: ctx.Err()}
	})
	h.WaitReports(1, 5*time.Second)
	time.Sleep(100 * time.Millisecond)
	if reps := h.Reports(); len(reps) != 1 || reps[0].Status != engine.DeliveryFailed || reps[0].Error != "timeout after 50ms" {
		t.Fatalf("reports %+v", reps)
	}
}

func TestRunnerStopAbandonsPendingAttempts(t *testing.T) {
	h := enginetest.NewHost(t)
	r := delivery.NewRunner(h, 1)
	target := engine.Target{ID: "T1"}
	started := make(chan struct{})
	r.Go(dispatch(engine.DeliveryPolicy{Timeout: time.Minute}, target), target, func(ctx context.Context, _ int) delivery.Attempt {
		close(started)
		<-ctx.Done()
		return delivery.Attempt{Err: ctx.Err()}
	})
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	r.Stop(ctx)
	if ctx.Err() != nil {
		t.Fatal("stop waited for the attempt's timeout")
	}
	if reps := h.Reports(); len(reps) != 0 {
		t.Fatalf("an abandoned attempt was reported: %+v", reps)
	}
}

func TestRunnerFailReportsWithoutTrying(t *testing.T) {
	h := enginetest.NewHost(t)
	r := delivery.NewRunner(h, 1)
	defer r.Stop(context.Background())
	target := engine.Target{ID: "T1"}
	r.Fail(dispatch(engine.DeliveryPolicy{Retries: 3}, target), target, errors.New("template: boom"))
	reps := h.Reports()
	if len(reps) != 1 || reps[0].Status != engine.DeliveryFailed || !strings.Contains(reps[0].Error, "boom") {
		t.Fatalf("reports %+v", reps)
	}
}
