package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"time"

	"github.com/corticoide/mockvision/backend/internal/engines/timefmt"
	"github.com/corticoide/mockvision/sdk/engine"
)

// The SD card's handlers (D68): the device's search of its recordings and
// their download, from the card or the NAS share the camera records to.

const (
	defaultSearchLimit = 100
	maxSearchLimit     = 1000
	filesTimeout       = 10 * time.Second
)

// FileResult is a recording as sd.search gives it to templates (.Result).
type FileResult struct {
	Name   string `json:"name"`
	Kind   string `json:"kind"`
	Event  string `json:"event"`
	Stream string `json:"stream,omitempty"`
	Size   int64  `json:"size"`
	// Start and End are written in the action's time format.
	Start     string    `json:"start"`
	End       string    `json:"end"`
	Duration  int       `json:"duration"` // seconds
	StartTime time.Time `json:"-"`
	EndTime   time.Time `json:"-"`
}

// errStatus carries the status a handler answers an error with.
type errStatus struct {
	status int
	msg    string
}

func (e *errStatus) Error() string { return e.msg }

func failWith(status int, format string, args ...any) error {
	return &errStatus{status: status, msg: fmt.Sprintf(format, args...)}
}

// storageError turns what the storage answers into a status.
func storageError(err error) error {
	switch {
	case errors.Is(err, engine.ErrNoFile):
		return failWith(http.StatusNotFound, "no such file")
	case errors.Is(err, engine.ErrStorageUnavailable):
		return failWith(http.StatusServiceUnavailable, "storage unavailable")
	}
	return failWith(http.StatusInternalServerError, "storage error")
}

// params reads the request's parameters from the query, the form or a
// JSON object.
func params(r *http.Request, from string, req *engine.RequestData) (url.Values, error) {
	switch from {
	case "form":
		v, err := url.ParseQuery(req.Body)
		if err != nil {
			return nil, failWith(http.StatusBadRequest, "invalid form body")
		}
		return v, nil
	case "json":
		var obj map[string]any
		if err := json.Unmarshal([]byte(req.Body), &obj); err != nil {
			return nil, failWith(http.StatusBadRequest, "body must be a JSON object")
		}
		v := url.Values{}
		for k, x := range obj {
			v.Set(k, fmt.Sprint(x))
		}
		return v, nil
	}
	return r.URL.Query(), nil
}

func paramName(a Action, key string) string {
	if name := a.Params[key]; name != "" {
		return name
	}
	return key
}

// sdSearch lists the recordings that overlap the requested time range.
func (e *Engine) sdSearch(r *http.Request, a Action, req *engine.RequestData) ([]FileResult, error) {
	files := e.in.Host.Files()
	if files == nil {
		return nil, failWith(http.StatusServiceUnavailable, "no storage")
	}
	v, err := params(r, a.From, req)
	if err != nil {
		return nil, err
	}
	var q engine.FileQuery
	if s := v.Get(paramName(a, "start")); s != "" {
		if q.From, err = timefmt.Parse(a.TimeFormat, s); err != nil {
			return nil, failWith(http.StatusBadRequest, "invalid start time")
		}
	}
	if s := v.Get(paramName(a, "end")); s != "" {
		if q.To, err = timefmt.Parse(a.TimeFormat, s); err != nil {
			return nil, failWith(http.StatusBadRequest, "invalid end time")
		}
	}
	if !q.From.IsZero() && !q.To.IsZero() && q.To.Before(q.From) {
		return nil, failWith(http.StatusBadRequest, "the end is before the start")
	}
	if k := v.Get(paramName(a, "kind")); k != "" {
		switch k {
		case a.Kinds["snapshot"], engine.FileSnapshot:
			q.Kind = engine.FileSnapshot
		case a.Kinds["clip"], engine.FileClip:
			q.Kind = engine.FileClip
		default:
			return nil, failWith(http.StatusBadRequest, "unknown kind %s", k)
		}
	}
	q.Event = v.Get(paramName(a, "event"))
	q.Limit = defaultSearchLimit
	if s := v.Get(paramName(a, "limit")); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 {
			return nil, failWith(http.StatusBadRequest, "invalid limit")
		}
		q.Limit = min(n, maxSearchLimit)
	}
	ctx, cancel := context.WithTimeout(r.Context(), filesTimeout)
	defer cancel()
	found, err := files.Find(ctx, q)
	if err != nil {
		return nil, storageError(err)
	}
	out := make([]FileResult, 0, len(found))
	for _, f := range found {
		kind := f.Kind
		if w := a.Kinds[kind]; w != "" {
			kind = w
		}
		out = append(out, FileResult{Name: f.Name, Kind: kind, Event: f.Event, Stream: f.Stream, Size: f.Size,
			Start: timefmt.Format(a.TimeFormat, f.Start), End: timefmt.Format(a.TimeFormat, f.End),
			Duration: int(f.Duration() / time.Second), StartTime: f.Start, EndTime: f.End})
	}
	return out, nil
}

// sdDownload sends a recording by its name.
func (e *Engine) sdDownload(w *countingWriter, r *http.Request, a Action, req *engine.RequestData) error {
	files := e.in.Host.Files()
	if files == nil {
		return failWith(http.StatusServiceUnavailable, "no storage")
	}
	v, err := params(r, a.From, req)
	if err != nil {
		return err
	}
	key := a.Key
	if key == "" {
		key = "name"
	}
	name := v.Get(key)
	if name == "" {
		return failWith(http.StatusBadRequest, "no file named")
	}
	ctx, cancel := context.WithTimeout(r.Context(), filesTimeout)
	defer cancel()
	rc, info, err := files.Open(ctx, name)
	if err != nil {
		return storageError(err)
	}
	defer rc.Close()
	ct := "image/jpeg"
	if info.Kind == engine.FileClip {
		ct = "video/mp2t"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size, 10))
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", path.Base(info.Name)))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		// The whole file, read as it goes: a large clip is not held in
		// memory.
		_, _ = io.Copy(w, rc)
	}
	return nil
}
