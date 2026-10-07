// Package ftpupload implements the ftp-upload engine: the camera uploads a
// file for each event, its snapshot or a document its profile renders, to
// FTP servers in passive mode and to SFTP servers (D31). The directory and
// file name come from the profile's templates, under the target's own
// directory.
package ftpupload

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"github.com/corticoide/mockvision/backend/internal/engines/delivery"
	"github.com/corticoide/mockvision/sdk/engine"
)

// Name and Version identify the engine in profiles (ftp-upload@^1).
const (
	Name    = "ftp-upload"
	Version = "1.0.0"
)

// Transport is the name of the transport this engine delivers.
const Transport = engine.TransportFTP

// What an upload carries.
const (
	ContentSnapshot = "snapshot"
	ContentBody     = "body"
)

const configSchema = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "additionalProperties": false,
  "required": ["engine"],
  "properties": {
    "engine": {"type": "string"},
    "max_parallel": {"type": "integer", "minimum": 1, "maximum": 16},
    "ssh_client_version": {"type": "string", "pattern": "^SSH-2\\.0-[!-~]{1,200}$"}
  }
}`

type config struct {
	MaxParallel      int    `json:"max_parallel"`
	SSHClientVersion string `json:"ssh_client_version"`
}

// transportConfig is the ftp section of an event.
type transportConfig struct {
	Path    string `json:"path"`
	File    string `json:"file"`
	Content string `json:"content"`
	Stream  string `json:"stream"`
	Body    string `json:"body"`
}

// maxFile bounds an uploaded document; snapshots are bounded by the media.
const maxFile = 8 << 20

// Engine uploads the events of one camera.
type Engine struct {
	in        engine.StartInput
	cfg       config
	runner    *delivery.Runner
	templates *delivery.Templates
	unsub     func()
	state     atomic.Value
}

// New returns an engine instance.
func New() engine.Engine {
	e := &Engine{}
	e.state.Store(engine.HealthStopped)
	return e
}

// Describe implements engine.Engine.
func (e *Engine) Describe() engine.Descriptor {
	return engine.Descriptor{
		Name:         Name,
		Version:      Version,
		Contract:     engine.Contract,
		Role:         engine.RoleClient,
		ConfigSchema: json.RawMessage(configSchema),
		Delivers:     []string{Transport},
	}
}

// Validate implements engine.Engine.
func (e *Engine) Validate(json.RawMessage) []engine.Problem { return nil }

// Start implements engine.Engine.
func (e *Engine) Start(_ context.Context, in engine.StartInput) error {
	e.in = in
	if err := json.Unmarshal(in.Config, &e.cfg); err != nil {
		return err
	}
	if e.cfg.MaxParallel <= 0 {
		e.cfg.MaxParallel = 2
	}
	e.runner = delivery.NewRunner(in.Host.Events(), e.cfg.MaxParallel)
	e.templates = delivery.NewTemplates(in.Host.Templates())
	e.unsub = in.Host.Events().Subscribe(Transport, e.dispatch)
	e.state.Store(engine.HealthOK)
	return nil
}

// Reload implements engine.Engine.
func (e *Engine) Reload(_ context.Context, raw json.RawMessage) error {
	var c config
	return json.Unmarshal(raw, &c)
}

// Health implements engine.Engine.
func (e *Engine) Health() engine.Health {
	if e.runner == nil {
		return engine.Health{State: engine.HealthStopped}
	}
	return e.runner.Health(e.state.Load().(engine.HealthState))
}

// Stop implements engine.Engine; pending uploads are abandoned.
func (e *Engine) Stop(ctx context.Context) error {
	if e.unsub != nil {
		e.unsub()
	}
	if e.runner != nil {
		e.runner.Stop(ctx)
	}
	e.state.Store(engine.HealthStopped)
	return nil
}

func (e *Engine) dispatch(d engine.Dispatch) {
	var tc transportConfig
	if err := json.Unmarshal(d.Transport, &tc); err != nil {
		e.in.Host.Telemetry().Log(slog.LevelError, "ftp: invalid transport", "error", err)
		return
	}
	dir, name, data, err := e.file(d, tc)
	for _, target := range d.Targets {
		if target.Type != engine.TargetFTP && target.Type != engine.TargetSFTP {
			continue
		}
		if err != nil {
			e.runner.Fail(d, target, err)
			continue
		}
		e.runner.Go(d, target, func(ctx context.Context, _ int) delivery.Attempt {
			if err := Upload(ctx, target, dir, name, data, e.cfg.SSHClientVersion); err != nil {
				return delivery.Attempt{Err: err}
			}
			return delivery.Attempt{Bytes: len(data)}
		})
	}
}

// file renders what an event uploads: the directory under the target's,
// the file name and the content.
func (e *Engine) file(d engine.Dispatch, tc transportConfig) (dir, name string, data []byte, err error) {
	ctx := e.runner.Context()
	td := delivery.Data(e.in.Identity, d)
	if tc.Path != "" {
		raw, err := e.templates.Render(ctx, "path", tc.Path, 0, td)
		if err != nil {
			return "", "", nil, fmt.Errorf("template: %w", err)
		}
		if dir, err = cleanDir(string(raw)); err != nil {
			return "", "", nil, err
		}
	}
	file := tc.File
	if file == "" {
		file = `{{ fmtTime .Event.At "20060102150405" }}_{{ .EventName }}.jpg`
	}
	raw, err := e.templates.Render(ctx, "file", file, 0, td)
	if err != nil {
		return "", "", nil, fmt.Errorf("template: %w", err)
	}
	if name, err = cleanName(string(raw)); err != nil {
		return "", "", nil, err
	}
	switch tc.Content {
	case ContentBody:
		data, err = e.templates.Render(ctx, Transport, tc.Body, maxFile, td)
		if err != nil {
			return "", "", nil, fmt.Errorf("template: %w", err)
		}
	default:
		stream := tc.Stream
		if stream == "" {
			stream = "main"
		}
		if data, err = e.in.Host.Media().Snapshot(stream); err != nil {
			return "", "", nil, fmt.Errorf("snapshot: %w", err)
		}
	}
	return dir, name, data, nil
}

// cleanDir checks a rendered directory: relative, no step out of the
// target's directory, no control characters. Empty segments, as an empty
// value leaves, are dropped.
func cleanDir(s string) (string, error) {
	var segs []string
	for _, seg := range strings.Split(strings.TrimSpace(s), "/") {
		if seg == "" {
			continue
		}
		if err := checkSegment(seg); err != nil {
			return "", fmt.Errorf("upload path %q: %w", s, err)
		}
		segs = append(segs, seg)
	}
	return strings.Join(segs, "/"), nil
}

func cleanName(s string) (string, error) {
	s = strings.TrimSpace(s)
	if strings.Contains(s, "/") {
		return "", fmt.Errorf("file name %q must not contain /", s)
	}
	if err := checkSegment(s); err != nil {
		return "", fmt.Errorf("file name %q: %w", s, err)
	}
	return s, nil
}

func checkSegment(seg string) error {
	switch {
	case seg == "" || seg == "." || seg == "..":
		return errors.New("empty, . or .. segment")
	case len(seg) > 255:
		return errors.New("segment longer than 255 bytes")
	case strings.ContainsFunc(seg, func(r rune) bool { return r < 0x20 || r == 0x7f || r == '\\' }):
		return errors.New("control characters or backslashes")
	}
	return nil
}

// Location is where a target's URL puts uploads.
type Location struct {
	HostPort string
	// Dir is the target's directory. FTP: relative to the login directory
	// unless it starts with /, written %2F in the URL. SFTP: absolute, or
	// relative to the home directory after /~/. As curl reads them.
	Dir string
}

// Locate parses ftp://host[:21]/dir or sftp://host[:22]/dir.
func Locate(t engine.Target) (Location, error) {
	u, err := url.Parse(t.URL)
	if err != nil || u.Hostname() == "" {
		return Location{}, fmt.Errorf("URL must look like %s://host/dir", t.Type)
	}
	port := "21"
	if u.Scheme == "sftp" {
		port = "22"
	}
	if p := u.Port(); p != "" {
		if n, err := strconv.Atoi(p); err != nil || n < 1 || n > 65535 {
			return Location{}, fmt.Errorf("invalid port %q", p)
		}
		port = p
	}
	loc := Location{HostPort: net.JoinHostPort(u.Hostname(), port)}
	p := u.Path
	switch u.Scheme {
	case "ftp":
		p = strings.TrimPrefix(p, "/")
	case "sftp":
		if rest, ok := strings.CutPrefix(p, "/~/"); ok {
			p = rest
		} else if p == "/~" {
			p = ""
		}
	}
	if p != "" && p != "/" {
		abs := strings.HasPrefix(p, "/")
		clean, err := cleanDir(p)
		if err != nil {
			return Location{}, err
		}
		if abs {
			clean = "/" + clean
		}
		loc.Dir = clean
	} else if p == "/" {
		loc.Dir = "/"
	}
	return loc, nil
}

// Upload stores data as dir/name under the target's directory.
func Upload(ctx context.Context, t engine.Target, dir, name string, data []byte, sshVersion string) error {
	loc, err := Locate(t)
	if err != nil {
		return err
	}
	full := joinDir(loc.Dir, dir)
	if t.Type == engine.TargetSFTP {
		return uploadSFTP(ctx, t, loc, full, name, data, sshVersion)
	}
	c, err := dialFTP(ctx, loc.HostPort, t.Username, t.Password)
	if err != nil {
		return err
	}
	defer c.Close()
	if err := c.chdirAll(full); err != nil {
		return err
	}
	return c.store(ctx, name, data)
}

func joinDir(base, rel string) string {
	switch {
	case rel == "":
		return base
	case base == "" || base == "/":
		return base + rel
	}
	return base + "/" + rel
}

// Probe logs in and enters the target's directory, creating it: the
// connection test of the panel. Nothing is uploaded.
func Probe(ctx context.Context, t engine.Target) error {
	loc, err := Locate(t)
	if err != nil {
		return err
	}
	if t.Type == engine.TargetSFTP {
		client, closeAll, err := dialSFTP(ctx, t, loc, "")
		if err != nil {
			return err
		}
		defer closeAll()
		if loc.Dir != "" {
			return client.MkdirAll(loc.Dir)
		}
		_, err = client.Getwd()
		return err
	}
	c, err := dialFTP(ctx, loc.HostPort, t.Username, t.Password)
	if err != nil {
		return err
	}
	defer c.Close()
	return c.chdirAll(loc.Dir)
}

func uploadSFTP(ctx context.Context, t engine.Target, loc Location, dir, name string, data []byte, sshVersion string) error {
	client, closeAll, err := dialSFTP(ctx, t, loc, sshVersion)
	if err != nil {
		return err
	}
	defer closeAll()
	file := name
	if dir != "" {
		if err := client.MkdirAll(dir); err != nil {
			return fmt.Errorf("cannot create %s: %w", dir, err)
		}
		file = path.Join(dir, name)
	}
	f, err := client.OpenFile(file, os.O_WRONLY|os.O_CREATE|os.O_TRUNC)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// dialSFTP opens an SSH connection with the target's password and an SFTP
// session over it. A pinned host key must match; without one any key is
// accepted, as most cameras do.
func dialSFTP(ctx context.Context, t engine.Target, loc Location, version string) (*sftp.Client, func(), error) {
	conn, err := delivery.Dial(ctx, "tcp", loc.HostPort)
	if err != nil {
		return nil, nil, err
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	var keyErr error
	cfg := &ssh.ClientConfig{
		User: t.Username,
		Auth: []ssh.AuthMethod{
			ssh.Password(t.Password),
			ssh.KeyboardInteractive(func(_, _ string, questions []string, _ []bool) ([]string, error) {
				answers := make([]string, len(questions))
				for i := range answers {
					answers[i] = t.Password
				}
				return answers, nil
			}),
		},
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			if t.HostKey == "" {
				return nil
			}
			if got := ssh.FingerprintSHA256(key); got != t.HostKey {
				keyErr = fmt.Errorf("the server's host key is %s, not the pinned %s", got, t.HostKey)
				return keyErr
			}
			return nil
		},
		ClientVersion: version,
		Timeout:       5 * time.Second,
	}
	sc, chans, reqs, err := ssh.NewClientConn(conn, loc.HostPort, cfg)
	if err != nil {
		conn.Close()
		if keyErr != nil {
			return nil, nil, keyErr
		}
		return nil, nil, err
	}
	client := ssh.NewClient(sc, chans, reqs)
	s, err := sftp.NewClient(client)
	if err != nil {
		client.Close()
		return nil, nil, err
	}
	return s, func() { s.Close(); client.Close() }, nil
}
