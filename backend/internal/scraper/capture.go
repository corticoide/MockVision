package scraper

import (
	"context"
	"encoding/base64"
	"net/http"
	"time"
	"unicode/utf8"
)

// CaptureResult is what running a program against a device produced: the
// recordings and a short summary. The recordings are not yet a profile;
// feature 20 sanitizes and compiles them.
type CaptureResult struct {
	Program  string    `json:"program"`
	Vendor   string    `json:"vendor,omitempty"`
	Fixtures []Fixture `json:"fixtures"`
	Steps    int       `json:"steps"`
	OK       int       `json:"ok"`
}

// RunProgram runs each read-only step of a program against the device and
// records what it answered. A step that fails is recorded with its error
// and the capture goes on.
func RunProgram(ctx context.Context, p *Prober, prog *Program) CaptureResult {
	res := CaptureResult{Program: prog.ID, Steps: len(prog.Steps)}
	for _, step := range prog.Steps {
		if ctx.Err() != nil {
			break
		}
		f := Fixture{StepID: step.ID, Kind: step.Kind, Port: step.Port, Path: step.Path, Query: step.Query, Vary: step.Vary,
			Stream: step.Stream, Binary: step.Binary, At: time.Now().UTC()}
		switch step.Kind {
		case "http":
			resp, _, err := p.HTTP(ctx, step.Method, step.Port, step.query(), step.Auth)
			f.Method = step.Method
			if err != nil {
				f.Error = err.Error()
				break
			}
			f.Status = resp.Status
			f.ContentType = resp.Header.Get("Content-Type")
			f.Headers = pickHeaders(resp.Header)
			f.Bytes = len(resp.Body)
			if step.Binary || !utf8.Valid(resp.Body) {
				f.Binary = true
				f.Body = base64.StdEncoding.EncodeToString(resp.Body)
			} else {
				f.Body = string(resp.Body)
			}
		case "rtsp":
			f.Method = step.Method
			if step.Method == "DESCRIBE" {
				r, ok := p.Describe(ctx, step.Port, step.Path)
				if !ok {
					f.Error = "no answer"
					break
				}
				f.Status = r.Status
				f.ContentType = "application/sdp"
				f.Body = r.Body
				f.Bytes = len(r.Body)
			} else {
				if s, ok := p.rtspOptions(ctx, step.Port); ok {
					f.Status, f.Body = 200, s
				} else {
					f.Error = "no answer"
				}
			}
		}
		if f.Error == "" && f.Status > 0 && f.Status < 400 {
			res.OK++
		}
		res.Fixtures = append(res.Fixtures, f)
	}
	return res
}

// recordedHeaders are the response headers a fixture keeps: enough to
// replay the answer, nothing that carries a secret.
var recordedHeaders = []string{"Server", "Content-Type", "Public", "Allow", "Cache-Control"}

func pickHeaders(h http.Header) map[string]string {
	out := map[string]string{}
	for _, k := range recordedHeaders {
		if v := h.Get(k); v != "" {
			out[k] = v
		}
	}
	return out
}
