package ipc

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"strings"
	"testing"
	"time"
)

func pair(t *testing.T, ha, hb Handler) (*Conn, *Conn) {
	t.Helper()
	a, b := net.Pipe()
	ca, cb := NewConn(a, ha, nil), NewConn(b, hb, nil)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go ca.Run(ctx)
	go cb.Run(ctx)
	return ca, cb
}

func TestRequestReply(t *testing.T) {
	server := func(_ context.Context, m *Envelope) (any, error) {
		switch m.Type {
		case TypeTrigger:
			var tr Trigger
			if err := m.Decode(&tr); err != nil {
				return nil, err
			}
			return map[string]string{"got": tr.Type}, nil
		default:
			return nil, Errorf("unsupported", "unknown type %s", m.Type)
		}
	}
	a, _ := pair(t, nil, server)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var out map[string]string
	if err := a.Request(ctx, TypeTrigger, Trigger{Type: "line_crossing"}, &out); err != nil {
		t.Fatal(err)
	}
	if out["got"] != "line_crossing" {
		t.Fatalf("reply = %v", out)
	}
	err := a.Request(ctx, "nope", nil, nil)
	if ie, ok := err.(*Error); !ok || ie.Code != "unsupported" {
		t.Fatalf("expected an ipc error, got %v", err)
	}
}

func TestHandlerCanRequest(t *testing.T) {
	// The camera handles configure by asking the service something first:
	// replies must still flow while the handler waits.
	var service *Conn
	cameraHandler := func(ctx context.Context, m *Envelope) (any, error) {
		return nil, service.Request(ctx, TypeLog, Log{Msg: "hi"}, nil)
	}
	serviceHandler := func(context.Context, *Envelope) (any, error) { return nil, nil }
	var camera *Conn
	camera, service = pair(t, serviceHandler, cameraHandler)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := camera.Request(ctx, TypeConfigure, Configure{}, nil); err != nil {
		t.Fatalf("nested request deadlocked or failed: %v", err)
	}
}

func TestOversizedAndBadVersion(t *testing.T) {
	a, b := net.Pipe()
	got := make(chan string, 4)
	conn := NewConn(b, func(_ context.Context, m *Envelope) (any, error) {
		got <- m.Type
		return nil, nil
	}, nil)
	go conn.Run(context.Background())
	defer a.Close()

	go func() {
		w := bufio.NewWriter(a)
		w.WriteString(`{"v":1,"id":"x","type":"` + strings.Repeat("a", MaxMessageSize) + `"}` + "\n")
		w.WriteString(`{"v":2,"id":"y","type":"old"}` + "\n")
		w.WriteString(`{"v":1,"type":"heartbeat"}` + "\n")
		w.Flush()
	}()
	r := bufio.NewReader(a)
	line, err := r.ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	var rep Envelope
	if err := json.Unmarshal(line, &rep); err != nil || rep.Re != "y" || rep.Error == nil {
		t.Fatalf("expected a version error reply, got %s", line)
	}
	select {
	case typ := <-got:
		if typ != "heartbeat" {
			t.Fatalf("oversized message must be dropped, got %s", typ)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("valid message after the oversized one was not handled")
	}
}
