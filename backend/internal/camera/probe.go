package camera

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/corticoide/mockvision/backend/internal/buildinfo"
	"github.com/corticoide/mockvision/backend/internal/ipc"
	"github.com/corticoide/mockvision/sdk/engine"
)

// probeTarget sends a test request to a target from the camera's network,
// the way its deliveries reach it (audit B7).
func (r *Runtime) probeTarget(ctx context.Context, t engine.Target) ipc.TargetTestResult {
	body, _ := json.Marshal(map[string]any{"test": true, "source": "MockVision", "camera": r.identity.Name, "target": t.Name, "at": time.Now().UTC()})
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	method := t.Method
	if method == "" {
		method = http.MethodPost
	}
	var reader io.Reader
	if method != http.MethodGet {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, t.URL, reader)
	if err != nil {
		return ipc.TargetTestResult{Error: err.Error()}
	}
	if reader != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("User-Agent", buildinfo.UserAgent())
	for k, v := range t.Headers {
		req.Header.Set(k, v)
	}
	if t.Username != "" {
		req.SetBasicAuth(t.Username, t.Password)
	}
	client := &http.Client{
		Transport:     &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 5 * time.Second}).DialContext},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	start := time.Now()
	resp, err := client.Do(req)
	res := ipc.TargetTestResult{LatencyMS: time.Since(start).Milliseconds()}
	if err != nil {
		var ue interface{ Unwrap() error }
		if errors.As(err, &ue) && ue.Unwrap() != nil {
			err = ue.Unwrap()
		}
		res.Error = err.Error()
		return res
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	res.HTTPStatus = resp.StatusCode
	res.OK = resp.StatusCode >= 200 && resp.StatusCode < 300
	if !res.OK {
		res.Error = fmt.Sprintf("target answered %d", resp.StatusCode)
	}
	return res
}
