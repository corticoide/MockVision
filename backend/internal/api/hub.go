package api

import (
	"encoding/json"
	"strings"
	"sync"
	"time"
)

// Message is what WebSocket clients receive.
type Message struct {
	Topic string          `json:"topic"`
	Seq   uint64          `json:"seq"`
	Type  string          `json:"type"`
	At    int64           `json:"at"`
	Data  json.RawMessage `json:"data,omitempty"`
}

// bufferSize is how many messages a topic keeps for reconnecting clients.
// Node metrics are only useful live, so that topic keeps one.
func bufferSize(topic string) int {
	switch {
	case topic == "node":
		return 1
	case strings.HasPrefix(topic, "camera:"), strings.HasPrefix(topic, "job:"):
		return 200
	}
	return 1000
}

// topics a client may subscribe to: the fixed ones, and one camera or job
// by its ID.
var fixedTopics = map[string]bool{"node": true, "cameras": true, "events": true, "profiles": true, "audit": true, "jobs": true}

func validTopic(name string) bool {
	if fixedTopics[name] {
		return true
	}
	for _, prefix := range []string{"camera:", "job:"} {
		if id, ok := strings.CutPrefix(name, prefix); ok {
			return validID(id)
		}
	}
	return false
}

// validID accepts the ULIDs the node uses as IDs.
func validID(id string) bool {
	if len(id) != 26 {
		return false
	}
	for _, r := range id {
		if !(r >= '0' && r <= '9' || r >= 'A' && r <= 'Z') {
			return false
		}
	}
	return true
}

type topicState struct {
	seq uint64
	buf []Message
}

// Hub fans messages out to WebSocket clients. Every topic numbers its
// messages; a reconnecting client sends the last seq it saw and gets what
// it missed, or a resync when the buffer no longer covers it.
type Hub struct {
	mu      sync.Mutex
	topics  map[string]*topicState
	clients map[*wsClient]struct{}
}

// NewHub returns an empty hub.
func NewHub() *Hub {
	return &Hub{topics: map[string]*topicState{}, clients: map[*wsClient]struct{}{}}
}

func (h *Hub) topic(name string) *topicState {
	t, ok := h.topics[name]
	if !ok {
		t = &topicState{}
		h.topics[name] = t
	}
	return t
}

// Publish implements app.Publisher.
func (h *Hub) Publish(topic, typ string, data any) {
	raw, err := json.Marshal(data)
	if err != nil {
		return
	}
	h.mu.Lock()
	t := h.topic(topic)
	t.seq++
	msg := Message{Topic: topic, Seq: t.seq, Type: typ, At: time.Now().UnixMilli(), Data: raw}
	t.buf = append(t.buf, msg)
	if max := bufferSize(topic); len(t.buf) > max {
		t.buf = append(t.buf[:0:0], t.buf[len(t.buf)-max:]...)
	}
	// Pushing never blocks, so it happens under the lock and every client
	// sees a topic's messages in seq order.
	for c := range h.clients {
		if c.subscribed(topic) {
			c.push(msg)
		}
	}
	h.mu.Unlock()
}

// Forget drops what a topic keeps for reconnecting clients, once nothing
// more will be published on it (a deleted camera, a finished job). A client
// that comes back to it sees a new sequence and reloads it over REST.
func (h *Hub) Forget(topic string) {
	h.mu.Lock()
	delete(h.topics, topic)
	h.mu.Unlock()
}

// disconnect ends the connections that match, for example those opened
// with a token just revoked or a session just closed.
func (h *Hub) disconnect(match func(*wsClient) bool, reason string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.clients {
		if match(c) {
			c.end(reason)
		}
	}
}

func (h *Hub) add(c *wsClient) {
	h.mu.Lock()
	h.clients[c] = struct{}{}
	h.mu.Unlock()
}

func (h *Hub) remove(c *wsClient) {
	h.mu.Lock()
	delete(h.clients, c)
	h.mu.Unlock()
}

// subscribe registers topics for a client and queues what it must receive
// first: missed messages, a resync notice, or the current seq. It queues
// under the hub lock so later messages cannot overtake the replay.
func (h *Hub) subscribe(c *wsClient, topics []string, since map[string]uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := time.Now().UnixMilli()
	for _, name := range topics {
		if !validTopic(name) || !c.addTopic(name) {
			continue
		}
		// Subscribing never creates a topic: only publishing does.
		t := h.topics[name]
		if t == nil {
			t = &topicState{}
		}
		last, resume := since[name]
		switch {
		case !resume || last >= t.seq:
			c.push(Message{Topic: name, Seq: t.seq, Type: "subscribed", At: now})
		case len(t.buf) > 0 && t.buf[0].Seq <= last+1:
			for _, m := range t.buf {
				if m.Seq > last {
					c.push(m)
				}
			}
		default:
			c.push(Message{Topic: name, Seq: t.seq, Type: "resync", At: now})
		}
	}
}
