package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// wsClient is one WebSocket connection.
type wsClient struct {
	mu     sync.Mutex
	topics map[string]bool
	send   chan Message
	gone   chan struct{}
	once   sync.Once
}

const (
	wsQueue        = 4096
	wsPingInterval = 20 * time.Second
	wsWriteTimeout = 10 * time.Second
	maxTopics      = 256
)

func newWSClient() *wsClient {
	return &wsClient{topics: map[string]bool{}, send: make(chan Message, wsQueue), gone: make(chan struct{})}
}

func (c *wsClient) subscribed(topic string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.topics[topic]
}

// addTopic subscribes the client to a topic, up to maxTopics in all.
func (c *wsClient) addTopic(topic string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.topics[topic] && len(c.topics) >= maxTopics {
		return false
	}
	c.topics[topic] = true
	return true
}

func (c *wsClient) removeTopic(topic string) {
	c.mu.Lock()
	delete(c.topics, topic)
	c.mu.Unlock()
}

// push queues a message; a client too slow to keep up is disconnected and
// will resynchronize when it reconnects.
func (c *wsClient) push(m Message) {
	select {
	case c.send <- m:
	default:
		c.kill()
	}
}

func (c *wsClient) kill() {
	c.once.Do(func() { close(c.gone) })
}

// clientOp is what clients send: subscribe or unsubscribe. The socket is
// read-only for commands; those always go through REST.
type clientOp struct {
	Op     string            `json:"op"`
	Topics []string          `json:"topics"`
	Since  map[string]uint64 `json:"since"`
}

// serveWS upgrades an authenticated request.
func (s *Server) serveWS(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: s.cfg.AllowedOrigins})
	if err != nil {
		return
	}
	conn.SetReadLimit(64 << 10)
	client := newWSClient()
	s.hub.add(client)
	defer s.hub.remove(client)
	defer client.kill()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	go func() {
		defer cancel()
		for {
			var op clientOp
			_, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			if json.Unmarshal(data, &op) != nil {
				continue
			}
			switch op.Op {
			case "subscribe":
				if len(op.Topics) > maxTopics {
					op.Topics = op.Topics[:maxTopics]
				}
				s.hub.subscribe(client, op.Topics, op.Since)
			case "unsubscribe":
				for _, t := range op.Topics {
					client.removeTopic(t)
				}
			}
		}
	}()

	ping := time.NewTicker(wsPingInterval)
	defer ping.Stop()
	for {
		select {
		case <-ctx.Done():
			conn.Close(websocket.StatusNormalClosure, "")
			return
		case <-client.gone:
			conn.Close(websocket.StatusPolicyViolation, "too slow; reconnect and resync")
			return
		case <-ping.C:
			pctx, pcancel := context.WithTimeout(ctx, wsWriteTimeout)
			err := conn.Ping(pctx)
			pcancel()
			if err != nil {
				return
			}
		case m := <-client.send:
			raw, err := json.Marshal(m)
			if err != nil {
				continue
			}
			wctx, wcancel := context.WithTimeout(ctx, wsWriteTimeout)
			err = conn.Write(wctx, websocket.MessageText, raw)
			wcancel()
			if err != nil {
				if !errors.Is(err, context.Canceled) {
					s.log.Debug("websocket write failed", "error", err)
				}
				return
			}
		}
	}
}
