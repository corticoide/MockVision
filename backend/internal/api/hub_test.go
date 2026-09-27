package api

import (
	"strings"
	"testing"
)

func drain(c *wsClient) []Message {
	var out []Message
	for {
		select {
		case m := <-c.send:
			out = append(out, m)
		default:
			return out
		}
	}
}

func TestHubTopics(t *testing.T) {
	const cam = "01J8Z3QK0000000000000000AB"
	for topic, want := range map[string]bool{
		"cameras": true, "jobs": true, "audit": true, "camera:" + cam: true, "job:" + cam: true,
		"camera:../x": false, "camera:" + strings.ToLower(cam): false, "whatever": false, "job:": false,
	} {
		if got := validTopic(topic); got != want {
			t.Errorf("validTopic(%q) = %v", topic, got)
		}
	}

	h := NewHub()
	c := newWSClient()
	h.add(c)
	h.subscribe(c, []string{"cameras", "nope", "camera:" + cam}, nil)
	msgs := drain(c)
	if len(msgs) != 2 || msgs[0].Type != "subscribed" || msgs[1].Topic != "camera:"+cam {
		t.Fatalf("subscribe: %+v", msgs)
	}
	if len(h.topics) != 0 {
		t.Fatalf("subscribing created %d topics", len(h.topics))
	}

	h.Publish("camera:"+cam, "status", map[string]string{"state": "running"})
	h.Publish("camera:"+cam, "status", map[string]string{"state": "stopped"})
	if msgs := drain(c); len(msgs) != 2 || msgs[1].Seq != 2 {
		t.Fatalf("published: %+v", msgs)
	}
	// A reconnecting client gets what it missed.
	c2 := newWSClient()
	h.subscribe(c2, []string{"camera:" + cam}, map[string]uint64{"camera:" + cam: 1})
	if msgs := drain(c2); len(msgs) != 1 || msgs[0].Seq != 2 {
		t.Fatalf("replay: %+v", msgs)
	}
	// After Forget the topic starts over; the seq mismatch makes the client resync.
	h.Forget("camera:" + cam)
	c3 := newWSClient()
	h.subscribe(c3, []string{"camera:" + cam}, map[string]uint64{"camera:" + cam: 2})
	if msgs := drain(c3); len(msgs) != 1 || msgs[0].Type != "subscribed" || msgs[0].Seq != 0 {
		t.Fatalf("after forget: %+v", msgs)
	}

	// A client holds at most maxTopics topics.
	c4 := newWSClient()
	var many []string
	for i := 0; i < maxTopics+10; i++ {
		id := []byte(cam)
		id[20], id[21], id[22] = byte('A'+i%26), byte('A'+i/26%26), byte('0'+i/676%10)
		many = append(many, "camera:"+string(id))
	}
	h.subscribe(c4, many, nil)
	if n := len(c4.topics); n != maxTopics {
		t.Fatalf("client holds %d topics", n)
	}
}
