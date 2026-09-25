package camera

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"sync"

	"github.com/corticoide/mockvision/backend/internal/ipc"
	"github.com/corticoide/mockvision/backend/internal/media"
	"github.com/corticoide/mockvision/sdk/engine"
)

// mediaStore holds the precoded streams of the camera and implements
// engine.Media.
type mediaStore struct {
	mu       sync.RWMutex
	streams  map[string]*loadedStream
	watchers map[int]func(string)
	next     int
}

type loadedStream struct {
	info     engine.StreamInfo
	source   *engine.VideoSource
	snapshot []byte
	gopPath  string
}

func newMediaStore() *mediaStore {
	return &mediaStore{streams: map[string]*loadedStream{}, watchers: map[int]func(string){}}
}

func loadStream(s ipc.Stream) (*loadedStream, error) {
	data, err := os.ReadFile(s.GOPPath)
	if err != nil {
		return nil, fmt.Errorf("stream %s: %w", s.Name, err)
	}
	src, err := media.ParseGOP(data)
	if err != nil {
		return nil, fmt.Errorf("stream %s: %w", s.Name, err)
	}
	info := engine.StreamInfo{Name: s.Name, Codec: s.Codec, Width: s.Width, Height: s.Height, FPS: s.FPS, GOP: s.GOP, Bitrate: s.Bitrate}
	src.Info = info
	snap, err := os.ReadFile(s.SnapshotPath)
	if err != nil {
		return nil, fmt.Errorf("stream %s snapshot: %w", s.Name, err)
	}
	return &loadedStream{info: info, source: src, snapshot: snap, gopPath: s.GOPPath}, nil
}

// replace loads the given streams and swaps them in, notifying watchers of
// the streams whose content changed.
func (m *mediaStore) replace(streams []ipc.Stream) error {
	loaded := map[string]*loadedStream{}
	for _, s := range streams {
		ls, err := loadStream(s)
		if err != nil {
			return err
		}
		loaded[s.Name] = ls
	}
	m.mu.Lock()
	var changed []string
	for name, ls := range loaded {
		if old, ok := m.streams[name]; ok && old.gopPath != ls.gopPath {
			changed = append(changed, name)
		}
		m.streams[name] = ls
	}
	watchers := make([]func(string), 0, len(m.watchers))
	for _, w := range m.watchers {
		watchers = append(watchers, w)
	}
	m.mu.Unlock()
	sort.Strings(changed)
	for _, name := range changed {
		for _, w := range watchers {
			w(name)
		}
	}
	return nil
}

func (m *mediaStore) Streams() []engine.StreamInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]engine.StreamInfo, 0, len(m.streams))
	for _, s := range m.streams {
		out = append(out, s.info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

var errNoStream = errors.New("stream not available")

func (m *mediaStore) Snapshot(stream string) ([]byte, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.streams[stream]
	if !ok {
		return nil, errNoStream
	}
	return s.snapshot, nil
}

func (m *mediaStore) Source(stream string) (*engine.VideoSource, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.streams[stream]
	if !ok {
		return nil, errNoStream
	}
	return s.source, nil
}

func (m *mediaStore) Watch(fn func(string)) func() {
	m.mu.Lock()
	id := m.next
	m.next++
	m.watchers[id] = fn
	m.mu.Unlock()
	return func() {
		m.mu.Lock()
		delete(m.watchers, id)
		m.mu.Unlock()
	}
}
