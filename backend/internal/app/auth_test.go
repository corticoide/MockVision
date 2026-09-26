package app

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/corticoide/mockvision/backend/internal/store/db"
)

func TestHashLimiterBoundsConcurrency(t *testing.T) {
	h := newHashLimiter()
	h.wait = 50 * time.Millisecond
	release := make(chan struct{})
	var running sync.WaitGroup
	for range maxConcurrentHashes {
		running.Add(1)
		go func() {
			_ = h.do(context.Background(), func() { running.Done(); <-release })
		}()
	}
	running.Wait()
	// Both slots are busy: the next caller waits, then gives up.
	if err := h.do(context.Background(), func() { t.Error("ran without a free slot") }); !errors.Is(err, ErrBusy) {
		t.Fatalf("want ErrBusy, got %v", err)
	}
	close(release)
	if err := h.do(context.Background(), func() {}); err != nil {
		t.Fatalf("a free slot must be used: %v", err)
	}
}

func TestSetupAnswersBeforeHashing(t *testing.T) {
	svc, _ := newBareService(t)
	adminActor(t, svc)
	// With both slots taken, a hash would fail with ErrBusy: the setup
	// must say it is done without computing one.
	for range maxConcurrentHashes {
		svc.hashes.slots <- struct{}{}
	}
	defer func() {
		for range maxConcurrentHashes {
			<-svc.hashes.slots
		}
	}()
	_, _, err := svc.Setup(context.Background(), "other", "another long password", "", "127.0.0.1", "")
	if !errors.Is(err, ErrSetupDone) {
		t.Fatalf("want ErrSetupDone, got %v", err)
	}
}

func TestLoginGuardLimitsEachAddress(t *testing.T) {
	g := newLoginGuard()
	now := time.Now()
	g.now = func() time.Time { return now }
	for i := range ipBurst {
		if err := g.allowIP("198.51.100.7"); err != nil {
			t.Fatalf("attempt %d refused: %v", i+1, err)
		}
	}
	var rl *RateLimitError
	if err := g.allowIP("198.51.100.7"); !errors.As(err, &rl) {
		t.Fatalf("want a rate limit, got %v", err)
	}
	// Another address is not affected; the same /64 is.
	if err := g.allowIP("198.51.100.8"); err != nil {
		t.Fatal(err)
	}
	for range ipBurst {
		_ = g.allowIP("2001:db8::1")
	}
	if err := g.allowIP("2001:db8::2"); err == nil {
		t.Fatal("addresses of one /64 share their attempts")
	}
	now = now.Add(time.Minute)
	if err := g.allowIP("198.51.100.7"); err != nil {
		t.Fatalf("attempts come back with time: %v", err)
	}
}

func TestLoginGuardIsNeverEmptiedByFlooding(t *testing.T) {
	g := newLoginGuard()
	for range 5 {
		g.fail("admin|203.0.113.9")
	}
	if _, locked := g.locked("admin|203.0.113.9"); !locked {
		t.Fatal("five failures lock the pair")
	}
	for i := range maxGuardEntries + 100 {
		g.fail(fmt.Sprintf("user%d|203.0.113.9", i))
	}
	if _, locked := g.locked("admin|203.0.113.9"); !locked {
		t.Fatal("flooding the guard cleared a lock")
	}
	if len(g.entries) > maxGuardEntries {
		t.Fatalf("the guard holds %d entries", len(g.entries))
	}
}

func TestLoginRateLimitedPerAddress(t *testing.T) {
	svc, _ := newBareService(t)
	adminActor(t, svc)
	var rl *RateLimitError
	for i := range ipBurst + 1 {
		_, _, err := svc.Login(context.Background(), fmt.Sprintf("nobody%d", i), "wrong password!", "192.0.2.44", "")
		if i < ipBurst && !errors.Is(err, ErrUnauthenticated) {
			t.Fatalf("attempt %d: %v", i+1, err)
		}
		if i == ipBurst && !errors.As(err, &rl) {
			t.Fatalf("attempts with other usernames must count too, got %v", err)
		}
	}
}

// A session in use is extended and says so, so the API can send the
// browser a cookie that lasts as long (audit B3).
func TestSessionExtensionIsReported(t *testing.T) {
	svc, _ := newBareService(t)
	adminActor(t, svc)
	ctx := context.Background()
	token, sess, err := svc.Login(ctx, "admin", "correct horse battery", "10.0.0.5", "test")
	if err != nil {
		t.Fatal(err)
	}
	if again, err := svc.Authenticate(ctx, token); err != nil || again.Extended {
		t.Fatalf("a fresh session needs no extension: %+v %v", again, err)
	}
	soon := time.Now().Add(time.Hour)
	if err := svc.store.W().ExtendSession(ctx, db.ExtendSessionParams{ID: tokenID(token), ExpiresAt: soon.UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	ext, err := svc.Authenticate(ctx, token)
	if err != nil || !ext.Extended || !ext.ExpiresAt.After(soon.Add(10*time.Hour)) {
		t.Fatalf("extension: %+v %v (was %s)", ext, err, sess.ExpiresAt)
	}
	if err := svc.CheckSession(ctx, token); err != nil {
		t.Fatal(err)
	}
	_ = svc.Logout(ctx, token)
	if err := svc.CheckSession(ctx, token); err == nil {
		t.Fatal("a logged out session is still valid")
	}
}
