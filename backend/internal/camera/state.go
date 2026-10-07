package camera

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/corticoide/mockvision/backend/internal/profile"
	"github.com/corticoide/mockvision/sdk/engine"
)

// stateStore holds the native parameters of the camera and implements
// engine.State. Values are validated against the profile; changes made by
// clients of the emulated API are reported to the service, which persists
// them and records their origin (RN-08).
type stateStore struct {
	model *profile.Model

	mu       sync.RWMutex
	values   map[string]any
	watchers map[int]func([]engine.Change)
	next     int

	report func([]engine.Change)
	// skew moves the clock, as a clock_skew fault does.
	skew func() time.Duration
}

func newStateStore(m *profile.Model, id engine.Identity, initial map[string]any, report func([]engine.Change), skew func() time.Duration) *stateStore {
	defaults := m.DefaultsFor(profile.CameraIdentity{Serial: id.Serial, Name: id.Name, Model: id.Model, MAC: id.MAC, IP: id.IP, Firmware: id.Firmware})
	s := &stateStore{model: m, values: defaults, watchers: map[int]func([]engine.Change){}, report: report, skew: skew}
	for k, v := range initial {
		p, ok := m.Doc.State[k]
		if !ok {
			continue
		}
		if cv, err := profile.Coerce(p, v); err == nil {
			s.values[k] = cv
		}
	}
	return s
}

func (s *stateStore) Get(key string) (any, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.values[key]
	return v, ok
}

func (s *stateStore) List(prefix string) []engine.Param {
	s.mu.RLock()
	defer s.mu.RUnlock()
	keys := make([]string, 0, len(s.values))
	for k := range s.values {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	out := make([]engine.Param, 0, len(keys))
	for _, k := range keys {
		p := s.model.Doc.State[k]
		out = append(out, engine.Param{Key: k, Type: p.Type, Value: s.values[k], Bind: p.Bind})
	}
	return out
}

func (s *stateStore) Set(_ context.Context, in map[string]any, origin engine.Origin) ([]engine.Change, error) {
	problems := map[string]string{}
	coerced := map[string]any{}
	for k, v := range in {
		p, ok := s.model.Doc.State[k]
		if !ok {
			problems[k] = "unknown parameter"
			continue
		}
		cv, err := profile.Coerce(p, v)
		if err != nil {
			problems[k] = err.Error()
			continue
		}
		coerced[k] = cv
	}
	if len(problems) > 0 {
		return nil, &engine.StateError{Problems: problems}
	}

	keys := make([]string, 0, len(coerced))
	for k := range coerced {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	s.mu.Lock()
	// The streams the change drives must hold together: a width the
	// stream has with the height it keeps.
	merged := make(map[string]any, len(s.values))
	for k, v := range s.values {
		merged[k] = v
	}
	for k, v := range coerced {
		merged[k] = v
	}
	if problems := s.model.CheckStreams(merged, keys); len(problems) > 0 {
		s.mu.Unlock()
		return nil, &engine.StateError{Problems: problems}
	}
	var changes []engine.Change
	for _, k := range keys {
		if fmt.Sprint(s.values[k]) == fmt.Sprint(coerced[k]) {
			continue
		}
		s.values[k] = coerced[k]
		changes = append(changes, engine.Change{Key: k, Value: coerced[k], Bind: s.model.Doc.State[k].Bind, Origin: origin})
	}
	watchers := make([]func([]engine.Change), 0, len(s.watchers))
	for _, w := range s.watchers {
		watchers = append(watchers, w)
	}
	s.mu.Unlock()

	if len(changes) == 0 {
		return nil, nil
	}
	for _, w := range watchers {
		w(changes)
	}
	if origin.Kind == engine.OriginClient && s.report != nil {
		s.report(changes)
	}
	return changes, nil
}

// Canon implements engine.State. time.offset, the camera clock's offset
// in seconds, includes a clock_skew fault.
func (s *stateStore) Canon(key string) (any, bool) {
	s.mu.RLock()
	v, ok := s.model.Canon(key, s.values)
	s.mu.RUnlock()
	if key == "time.offset" && s.skew != nil {
		if d := s.skew(); d != 0 {
			secs, _ := v.(int64)
			return secs + int64(d/time.Second), true
		}
	}
	return v, ok
}

func (s *stateStore) Watch(fn func([]engine.Change)) func() {
	s.mu.Lock()
	id := s.next
	s.next++
	s.watchers[id] = fn
	s.mu.Unlock()
	return func() {
		s.mu.Lock()
		delete(s.watchers, id)
		s.mu.Unlock()
	}
}

// replace overwrites values without validation side effects (reload).
func (s *stateStore) replace(values map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, v := range values {
		if p, ok := s.model.Doc.State[k]; ok {
			if cv, err := profile.Coerce(p, v); err == nil {
				s.values[k] = cv
			}
		}
	}
}
