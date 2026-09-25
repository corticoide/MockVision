package camera

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

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
}

func newStateStore(m *profile.Model, initial map[string]any, report func([]engine.Change)) *stateStore {
	s := &stateStore{model: m, values: m.Defaults(), watchers: map[int]func([]engine.Change){}, report: report}
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

func (s *stateStore) Canon(key string) (any, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.model.Canon(key, s.values)
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
