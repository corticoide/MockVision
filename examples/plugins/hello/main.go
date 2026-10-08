// Command hello is an example MockVision plugin: an engine that answers a
// line protocol on its port. A client gets a greeting with the camera's
// serial and a parameter's value; "set <value>" writes the parameter and
// "event" raises a motion event. It shows what a plugin reaches through
// its camera: the state, events and telemetry, as its permissions allow.
//
// Build it and package it (see README.md):
//
//	CGO_ENABLED=0 go build -o pkg/bin/linux-amd64/hello ./examples/plugins/hello
//	mockvision pkg build pkg -o hello.mvpkg
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/corticoide/mockvision/sdk/engine"
	"github.com/corticoide/mockvision/sdk/plugin"
)

func main() { os.Exit(plugin.Serve(&hello{})) }

// config is the engine's section of a profile.
type config struct {
	Greeting string `json:"greeting"`
	Param    string `json:"param"`
}

type hello struct {
	host     engine.Host
	cfg      atomic.Pointer[config]
	ln       net.Listener
	wg       sync.WaitGroup
	clients  atomic.Int64
	requests atomic.Uint64
}

func (h *hello) Describe() engine.Descriptor {
	return engine.Descriptor{
		Name: "hello", Version: "1.0.0", Contract: engine.Contract, Role: engine.RoleServer,
		ConfigSchema: json.RawMessage(`{"type":"object","properties":{"engine":{"type":"string"},"port":{"type":"integer"},` +
			`"greeting":{"type":"string","maxLength":64},"param":{"type":"string"}},"additionalProperties":false}`),
		Sockets: []engine.SocketSpec{{Name: "tcp", Network: "tcp", DefaultPort: 7000}},
		Emits:   []string{"motion"},
	}
}

func (h *hello) Validate(json.RawMessage) []engine.Problem { return nil }

func (h *hello) Start(_ context.Context, in engine.StartInput) error {
	if err := h.Reload(context.Background(), in.Config); err != nil {
		return err
	}
	ln, ok := in.Listeners["tcp"]
	if !ok {
		return fmt.Errorf("no tcp socket")
	}
	h.host, h.ln = in.Host, ln
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			h.wg.Add(1)
			go func() {
				defer h.wg.Done()
				h.serve(c, in.Identity)
			}()
		}
	}()
	return nil
}

func (h *hello) serve(c net.Conn, id engine.Identity) {
	defer c.Close()
	h.clients.Add(1)
	defer h.clients.Add(-1)
	ip, _, _ := net.SplitHostPort(c.RemoteAddr().String())
	h.host.Telemetry().Client("hello", ip, true)
	defer h.host.Telemetry().Client("hello", ip, false)
	cfg := h.cfg.Load()
	value, _ := h.host.State().Get(cfg.Param)
	fmt.Fprintf(c, "%s %s %v\n", cfg.Greeting, id.Serial, value)
	sc := bufio.NewScanner(c)
	for sc.Scan() {
		h.requests.Add(1)
		cmd, arg, _ := strings.Cut(strings.TrimSpace(sc.Text()), " ")
		switch cmd {
		case "set":
			if _, err := h.host.State().Set(context.Background(), map[string]any{cfg.Param: arg}, engine.Origin{Kind: engine.OriginClient, IP: ip}); err != nil {
				fmt.Fprintf(c, "DENIED %v\n", err)
				continue
			}
			fmt.Fprintln(c, "OK")
		case "event":
			ev, err := h.host.Events().Emit(context.Background(), engine.Event{Type: "motion"})
			if err != nil {
				fmt.Fprintf(c, "DENIED %v\n", err)
				continue
			}
			fmt.Fprintf(c, "EMITTED %s\n", ev.ID)
		case "crash":
			// For tests of the camera's supervision: the process dies.
			os.Exit(3)
		default:
			h.host.Telemetry().Gap("hello", ip, "unknown command "+cmd)
			fmt.Fprintln(c, "?")
		}
	}
}

func (h *hello) Reload(_ context.Context, raw json.RawMessage) error {
	cfg := config{Greeting: "HELLO", Param: "System.DeviceName"}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return err
		}
	}
	h.cfg.Store(&cfg)
	return nil
}

func (h *hello) Health() engine.Health {
	return engine.Health{State: engine.HealthOK, Clients: int(h.clients.Load()), Requests: h.requests.Load()}
}

func (h *hello) Stop(context.Context) error {
	if h.ln != nil {
		h.ln.Close()
	}
	return nil
}
