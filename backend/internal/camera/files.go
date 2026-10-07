package camera

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/corticoide/mockvision/backend/internal/domain"
	"github.com/corticoide/mockvision/backend/internal/engines/delivery"
	"github.com/corticoide/mockvision/backend/internal/ipc"
	"github.com/corticoide/mockvision/backend/internal/media"
	"github.com/corticoide/mockvision/backend/internal/nas"
	"github.com/corticoide/mockvision/backend/internal/profile"
	"github.com/corticoide/mockvision/sdk/engine"
)

// nasTimeout bounds connecting to the share and each write to it.
const nasTimeout = 30 * time.Second

// fileStore implements engine.Files: the recordings of the camera's SD
// card, which the service writes and the camera only reads, or of its NAS
// share, which the camera writes and reads itself (D68, D69).
type fileStore struct {
	rt *Runtime

	mu  sync.Mutex
	cfg ipc.Storage

	// nasMu serializes the share's use; share is nil until connected.
	nasMu  sync.Mutex
	share  nas.Share
	nasKey string // the share the connection is for
	nasErr string // why the last use failed
}

func newFileStore(rt *Runtime) *fileStore { return &fileStore{rt: rt} }

func (f *fileStore) kind() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cfg.Kind
}

func (f *fileStore) config() ipc.Storage {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cfg
}

func nasKey(n *ipc.NAS) string {
	if n == nil {
		return ""
	}
	return n.URL + "\x00" + n.Username + "\x00" + n.Password
}

// set replaces the storage. A card that fills up raises storage_full,
// when raise is set, the profile defines it and no fault forces it full;
// a new share is connected to at once, so the panel learns whether the
// camera reaches it.
func (f *fileStore) set(cfg ipc.Storage, raise bool) {
	f.mu.Lock()
	prev := f.cfg
	f.cfg = cfg
	f.mu.Unlock()
	if raise && cfg.Kind == string(domain.StorageSD) && prev.Kind == cfg.Kind &&
		cfg.Status.State == engine.StorageFull && prev.Status.State != engine.StorageFull && !f.rt.faults.has(domain.FaultSDFull) {
		f.raise(domain.EventStorageFull)
	}
	key := ""
	if cfg.Kind == string(domain.StorageNAS) {
		key = nasKey(cfg.NAS)
	}
	f.nasMu.Lock()
	if key != f.nasKey {
		f.closeShareLocked()
		f.nasKey = key
		f.nasErr = ""
	}
	f.nasMu.Unlock()
	if key != "" {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), nasTimeout)
			defer cancel()
			f.nasMu.Lock()
			defer f.nasMu.Unlock()
			if f.nasKey == key && f.share == nil {
				_, _ = f.shareLocked(ctx)
			}
		}()
	}
}

func (f *fileStore) raise(ev domain.EventType) {
	if _, defined := f.rt.model.Doc.Events[string(ev)]; !defined {
		return
	}
	if _, err := f.rt.events.Emit(context.Background(), engine.Event{Type: string(ev), Trigger: "device"}); err != nil {
		f.rt.tel.Log(slog.LevelWarn, "the storage event was not raised", "type", string(ev), "error", err.Error())
	}
}

// Status implements engine.Files.
func (f *fileStore) Status() engine.StorageStatus {
	cfg := f.config()
	st := cfg.Status
	if cfg.Kind == string(domain.StorageNAS) {
		f.nasMu.Lock()
		if f.nasErr != "" {
			st.State = engine.StorageError
		} else {
			st.State = engine.StoragePresent
		}
		f.nasMu.Unlock()
	}
	return st
}

// Find implements engine.Files: the service keeps the index.
func (f *fileStore) Find(ctx context.Context, q engine.FileQuery) ([]engine.FileInfo, error) {
	if !f.Status().Usable() {
		return nil, engine.ErrStorageUnavailable
	}
	var out ipc.FilesFound
	err := f.rt.conn.Request(ctx, ipc.TypeFilesFind, ipc.FilesFind{From: q.From, To: q.To, Kind: q.Kind, Event: q.Event, Limit: q.Limit}, &out)
	return out.Files, err
}

// Open implements engine.Files.
func (f *fileStore) Open(ctx context.Context, name string) (io.ReadCloser, engine.FileInfo, error) {
	if !domain.ValidRecordingName(name) {
		return nil, engine.FileInfo{}, engine.ErrNoFile
	}
	if !f.Status().Usable() {
		return nil, engine.FileInfo{}, engine.ErrStorageUnavailable
	}
	var found ipc.FilesFound
	if err := f.rt.conn.Request(ctx, ipc.TypeFilesFind, ipc.FilesFind{Name: name}, &found); err != nil {
		return nil, engine.FileInfo{}, err
	}
	if len(found.Files) == 0 {
		return nil, engine.FileInfo{}, engine.ErrNoFile
	}
	info := found.Files[0]
	cfg := f.config()
	switch cfg.Kind {
	case string(domain.StorageSD):
		file, err := os.Open(filepath.Join(cfg.SDDir, filepath.FromSlash(name)))
		if err != nil {
			return nil, info, engine.ErrNoFile
		}
		return file, info, nil
	case string(domain.StorageNAS):
		return &shareReader{f: f, name: name}, info, nil
	}
	return nil, info, engine.ErrNoFile
}

// shareReader reads a recording on the share from the start.
type shareReader struct {
	f    *fileStore
	name string
	off  int64
}

func (r *shareReader) Read(p []byte) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), nasTimeout)
	defer cancel()
	n, size, err := r.f.readAt(ctx, r.name, p[:min(len(p), ipc.MaxFileRead)], r.off)
	r.off += int64(n)
	if err != nil {
		return n, err
	}
	if r.off >= size {
		return n, io.EOF
	}
	return n, nil
}

func (r *shareReader) Close() error { return nil }

// folder is where the camera keeps its recordings on a share it may have
// with others: a directory named after its serial number.
func (f *fileStore) folder() string {
	if s := f.rt.identity.Serial; s != "" && domain.ValidRecordingName(s) {
		return s
	}
	return f.rt.identity.CameraID
}

// shareLocked returns the share, connecting to it when needed, and tells
// the service whether it worked; nasMu is held.
func (f *fileStore) shareLocked(ctx context.Context) (nas.Share, error) {
	if f.share != nil {
		return f.share, nil
	}
	cfg := f.config()
	if cfg.Kind != string(domain.StorageNAS) || cfg.NAS == nil {
		return nil, engine.ErrStorageUnavailable
	}
	s, err := nas.Open(ctx, cfg.NAS.URL, nas.Options{Username: cfg.NAS.Username, Password: cfg.NAS.Password, Machine: f.rt.identity.Name, Dial: delivery.Dial})
	if err != nil {
		f.failLocked(err)
		return nil, err
	}
	f.share = s
	f.nasErr = ""
	f.report(ipc.NASState{OK: true})
	return s, nil
}

// failLocked drops a share that failed: the next use connects again.
func (f *fileStore) failLocked(err error) {
	f.closeShareLocked()
	msg := delivery.Unwrap(err).Error()
	if f.nasErr != msg {
		f.rt.tel.Log(slog.LevelWarn, "the NAS share failed", "error", msg)
	}
	f.nasErr = msg
	f.report(ipc.NASState{OK: false, Error: msg})
}

func (f *fileStore) closeShareLocked() {
	if f.share != nil {
		_ = f.share.Close()
		f.share = nil
	}
}

func (f *fileStore) report(st ipc.NASState) {
	if err := f.rt.conn.Notify(ipc.TypeNASState, st); err != nil {
		f.rt.log.Debug("NAS state not reported", "error", err)
	}
}

// errOffline is what a camera off the network gets from its share.
var errOffline = errors.New("network is unreachable")

func (f *fileStore) readAt(ctx context.Context, name string, p []byte, off int64) (int, int64, error) {
	if !domain.ValidRecordingName(name) {
		return 0, 0, engine.ErrNoFile
	}
	if delivery.Offline() {
		return 0, 0, errOffline
	}
	f.nasMu.Lock()
	defer f.nasMu.Unlock()
	s, err := f.shareLocked(ctx)
	if err != nil {
		return 0, 0, err
	}
	n, size, err := s.ReadAt(ctx, f.folder()+"/"+name, p, off)
	if errors.Is(err, nas.ErrNotExist) {
		return 0, 0, engine.ErrNoFile
	}
	if err != nil {
		f.failLocked(err)
	}
	return n, size, err
}

// readFile answers the service's read of a recording on the share.
func (f *fileStore) readFile(ctx context.Context, rq ipc.FileRead) (ipc.FileData, error) {
	if f.kind() != string(domain.StorageNAS) {
		return ipc.FileData{}, ipc.Errorf("state", "the camera records to no share")
	}
	if rq.Length <= 0 || rq.Length > ipc.MaxFileRead || rq.Offset < 0 {
		return ipc.FileData{}, ipc.Errorf("invalid", "read between 1 and %d bytes", ipc.MaxFileRead)
	}
	buf := make([]byte, rq.Length)
	n, size, err := f.readAt(ctx, rq.Name, buf, rq.Offset)
	if errors.Is(err, engine.ErrNoFile) {
		return ipc.FileData{}, ipc.Errorf("not_found", "no such recording")
	}
	if err != nil {
		return ipc.FileData{}, ipc.Errorf("nas", "%v", err)
	}
	return ipc.FileData{Data: buf[:n], Size: size, EOF: rq.Offset+int64(n) >= size}, nil
}

// record writes what an event records to the share; the service records
// to the SD card.
func (f *fileStore) record(e engine.Event, spec profile.EventSpec) {
	if spec.Record == nil || f.kind() != string(domain.StorageNAS) {
		return
	}
	rec := *spec.Record
	go f.recordNAS(e, rec)
}

func (f *fileStore) recordNAS(e engine.Event, rec profile.RecordSpec) {
	stream := rec.Stream
	if stream == "" {
		stream = "main"
	}
	type file struct {
		kind string
		ext  string
		data []byte
		d    time.Duration
	}
	var files []file
	if rec.Snapshot {
		if jpeg, err := f.rt.media.Snapshot(stream); err == nil {
			files = append(files, file{engine.FileSnapshot, "jpg", jpeg, 0})
		}
	}
	if d := rec.Clip.D(); d > 0 {
		src, err := f.rt.media.Source(stream)
		if err == nil {
			var clip []byte
			if clip, err = media.Clip(src, min(d, media.MaxClip)); err == nil {
				files = append(files, file{engine.FileClip, "ts", clip, d})
			}
		}
		if err != nil {
			f.rt.tel.Log(slog.LevelWarn, "the event's clip was not recorded", "event", e.ID, "error", err.Error())
		}
	}
	for _, fl := range files {
		name := domain.RecordingName(e.Type, e.ID, e.At, fl.ext)
		if err := f.write(name, fl.data); err != nil {
			f.rt.tel.Log(slog.LevelWarn, "the recording did not reach the NAS", "event", e.ID, "error", err.Error())
			return
		}
		info := engine.FileInfo{Name: name, Kind: fl.kind, Event: e.Type, Stream: stream, Size: int64(len(fl.data)), Start: e.At, End: e.At.Add(fl.d)}
		if err := f.rt.conn.Notify(ipc.TypeRecorded, ipc.Recorded{EventID: e.ID, File: info}); err != nil {
			f.rt.log.Debug("recording not reported", "error", err)
		}
	}
}

func (f *fileStore) write(name string, data []byte) error {
	if delivery.Offline() {
		f.nasMu.Lock()
		f.failLocked(errOffline)
		f.nasMu.Unlock()
		return errOffline
	}
	ctx, cancel := context.WithTimeout(context.Background(), nasTimeout)
	defer cancel()
	f.nasMu.Lock()
	defer f.nasMu.Unlock()
	s, err := f.shareLocked(ctx)
	if err != nil {
		return err
	}
	if err := s.WriteFile(ctx, f.folder()+"/"+name, data); err != nil {
		f.failLocked(err)
		return err
	}
	return nil
}

// close drops the share as the camera stops.
func (f *fileStore) close() {
	f.nasMu.Lock()
	defer f.nasMu.Unlock()
	f.closeShareLocked()
}

// templateStatus is the storage as templates read it.
func (f *fileStore) templateStatus() any {
	st := f.Status()
	if st.Kind == "" {
		st.Kind = string(domain.StorageNone)
	}
	const mb = 1 << 20
	free := max(st.CapacityBytes-st.UsedBytes, 0)
	return map[string]any{"kind": st.Kind, "state": st.State, "capacity_mb": st.CapacityBytes / mb, "used_mb": st.UsedBytes / mb,
		"free_mb": free / mb, "files": st.Files, "overwrite": st.Overwrite}
}

var _ engine.Files = (*fileStore)(nil)
