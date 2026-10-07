package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/corticoide/mockvision/backend/internal/domain"
	"github.com/corticoide/mockvision/backend/internal/ipc"
	"github.com/corticoide/mockvision/backend/internal/media"
	"github.com/corticoide/mockvision/backend/internal/profile"
	"github.com/corticoide/mockvision/backend/internal/store"
	"github.com/corticoide/mockvision/backend/internal/store/db"
	"github.com/corticoide/mockvision/sdk/engine"
)

// Storage limits.
const (
	// sdReserve is the free space under which a card that does not
	// overwrite reports itself full.
	sdReserve = 2 << 20
	// maxRecordingSize bounds what a camera reports it wrote to its share.
	maxRecordingSize = 1 << 30
	// maxFilesFound bounds a search of the camera's API.
	maxFilesFound = 1000
	// recentRecordings is how many recordings the panel lists.
	recentRecordings = 200
	// maxSources is how many parsed streams the recorder keeps.
	maxSources = 4
)

// StorageInput changes where a camera records; nil fields stay.
type StorageInput struct {
	Kind        *string `json:"kind,omitempty"`
	SizeMB      *int    `json:"size_mb,omitempty"`
	Overwrite   *bool   `json:"overwrite,omitempty"`
	NASURL      *string `json:"nas_url,omitempty"`
	NASUsername *string `json:"nas_username,omitempty"`
	// NASPassword replaces the share's password; empty clears it.
	NASPassword *string `json:"nas_password,omitempty"`
}

// StorageView is where a camera records, what its model offers and the
// state of its card or share.
type StorageView struct {
	Kind           string `json:"kind"`
	SizeMB         int    `json:"size_mb"`
	Overwrite      bool   `json:"overwrite"`
	NASURL         string `json:"nas_url"`
	NASUsername    string `json:"nas_username"`
	HasNASPassword bool   `json:"has_nas_password"`
	// MaxSDMB is the largest card the model takes, 0 without a slot.
	MaxSDMB      int                  `json:"max_sd_mb"`
	NASProtocols []string             `json:"nas_protocols"`
	Status       engine.StorageStatus `json:"status"`
	// NASError is why the camera cannot reach its share.
	NASError string `json:"nas_error,omitempty"`
	// Records are the events the profile records, and what.
	Records []RecordView `json:"records"`
}

// RecordView is what an event type records.
type RecordView struct {
	Event    string `json:"event"`
	Snapshot bool   `json:"snapshot"`
	ClipS    int    `json:"clip_s"`
	Stream   string `json:"stream"`
}

// RecordingView is a recording of a camera.
type RecordingView struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Kind      string    `json:"kind"`
	Stream    string    `json:"stream,omitempty"`
	EventID   string    `json:"event_id,omitempty"`
	EventType string    `json:"event_type"`
	Size      int64     `json:"size"`
	Start     time.Time `json:"start"`
	End       time.Time `json:"end"`
	Location  string    `json:"location"`
}

// ContentType is the recording's media type.
func (r RecordingView) ContentType() string {
	if r.Kind == engine.FileClip {
		return "video/mp2t"
	}
	return "image/jpeg"
}

// storageState is what the service keeps in memory about the cameras'
// storage. write serializes what changes a card: recordings, a format, a
// new size.
type storageState struct {
	write sync.Mutex

	mu sync.Mutex
	// full marks the cards that refused a recording for want of space.
	full map[string]bool
	// nas is what each camera last said of its share.
	nas map[string]ipc.NASState
	// sources are parsed streams the recorder clips, by rendition key.
	sources map[string]*engine.VideoSource
}

func newStorageState() storageState {
	return storageState{full: map[string]bool{}, nas: map[string]ipc.NASState{}, sources: map[string]*engine.VideoSource{}}
}

func (st *storageState) forget(id string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	delete(st.full, id)
	delete(st.nas, id)
}

func (st *storageState) setFull(id string, full bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if full {
		st.full[id] = true
	} else {
		delete(st.full, id)
	}
}

// sdRoot holds the cards of the node, one directory each. Cameras read
// their own with their unprivileged user: they may traverse it, not list
// it.
func (s *Service) sdRoot() string { return filepath.Join(s.opts.DataDir, "sd") }

func (s *Service) sdDir(id string) string { return filepath.Join(s.sdRoot(), id) }

// ensureSDDir creates the camera's card directory, which its process may
// read whatever its storage is: it cannot reach new paths once confined.
func (s *Service) ensureSDDir(id string) error {
	if err := os.MkdirAll(s.sdRoot(), 0o711); err != nil {
		return err
	}
	_ = os.Chmod(s.sdRoot(), 0o711)
	if err := os.MkdirAll(s.sdDir(id), 0o755); err != nil {
		return err
	}
	// Whatever the service's umask: the camera's user reads it.
	return os.Chmod(s.sdDir(id), 0o755)
}

func storageOf(b *cameraBundle) domain.Storage {
	if b.storage == nil {
		return domain.Storage{Kind: domain.StorageNone, Overwrite: true}
	}
	st := b.storage
	return domain.Storage{Kind: domain.StorageKind(st.Kind), SizeMB: int(st.SizeMb), Overwrite: store.Bool(st.Overwrite),
		NASURL: st.NasUrl, NASUsername: st.NasUsername}
}

// sdFault is the state an SD fault forces on the camera's card, if any:
// a missing card first, then an error, read only and full.
func (s *Service) sdFault(ctx context.Context, id string) domain.SDState {
	open, err := s.openFaults(ctx, id)
	if err != nil {
		return ""
	}
	for _, kind := range []domain.FaultKind{domain.FaultSDMissing, domain.FaultSDError, domain.FaultSDReadOnly, domain.FaultSDFull} {
		if slices.ContainsFunc(open, func(f domain.Fault) bool { return f.Kind == kind }) {
			return domain.SDFaultStates[kind]
		}
	}
	return ""
}

// storageStatus is the camera's card or share as the device reports it.
func (s *Service) storageStatus(ctx context.Context, b *cameraBundle) engine.StorageStatus {
	st := storageOf(b)
	out := engine.StorageStatus{Kind: string(st.Kind), Overwrite: st.Overwrite}
	if st.Kind == domain.StorageNone {
		return out
	}
	if u, err := s.store.R().RecordingUsage(ctx, db.RecordingUsageParams{CameraID: b.cam.ID, Location: string(st.Kind)}); err == nil {
		out.UsedBytes, out.Files = u.Used, int(u.Files)
	}
	out.State = engine.StoragePresent
	switch st.Kind {
	case domain.StorageSD:
		out.CapacityBytes = int64(st.SizeMB) << 20
		if forced := s.sdFault(ctx, b.cam.ID); forced != "" {
			out.State = string(forced)
			return out
		}
		s.storage.mu.Lock()
		full := s.storage.full[b.cam.ID]
		s.storage.mu.Unlock()
		if !st.Overwrite && (full || out.CapacityBytes-out.UsedBytes < sdReserve) {
			out.State = engine.StorageFull
		}
	case domain.StorageNAS:
		s.storage.mu.Lock()
		ns, known := s.storage.nas[b.cam.ID]
		s.storage.mu.Unlock()
		if known && !ns.OK {
			out.State = engine.StorageError
		}
	}
	return out
}

// storageConfig is the storage a camera process gets.
func (s *Service) storageConfig(ctx context.Context, b *cameraBundle) (ipc.Storage, error) {
	st := storageOf(b)
	out := ipc.Storage{Kind: string(st.Kind), SDDir: s.sdDir(b.cam.ID), Status: s.storageStatus(ctx, b)}
	if st.Kind == domain.StorageNAS {
		nas := &ipc.NAS{URL: st.NASURL, Username: st.NASUsername}
		if len(b.storage.NasSecretEnc) > 0 {
			pw, err := s.box.Open(b.storage.NasSecretEnc, "camera_storage:"+b.cam.ID)
			if err != nil {
				return out, fmt.Errorf("cannot decrypt the NAS password: %w", err)
			}
			nas.Password = string(pw)
		}
		out.NAS = nas
	}
	return out, nil
}

// pushStorage tells a running camera its storage and its state, and the
// panel that they changed.
func (s *Service) pushStorage(ctx context.Context, id string) {
	defer s.pub.Publish("cameras", "storage", map[string]any{"camera_id": id})
	b, err := s.loadBundle(ctx, id)
	if err != nil {
		return
	}
	cfg, err := s.storageConfig(ctx, b)
	if err != nil {
		s.log.Warn("cannot send the camera its storage", "camera", id, "error", err)
		return
	}
	s.tellCamera(ctx, id, ipc.TypeStorage, cfg)
}

func recordViews(doc *profile.Document) []RecordView {
	out := []RecordView{}
	for _, typ := range profile.SortedKeys(doc.Events) {
		rec := doc.Events[typ].Record
		if rec == nil {
			continue
		}
		stream := rec.Stream
		if stream == "" {
			stream = "main"
		}
		out = append(out, RecordView{Event: typ, Snapshot: rec.Snapshot, ClipS: int(rec.Clip.D() / time.Second), Stream: stream})
	}
	return out
}

func (s *Service) storageView(ctx context.Context, b *cameraBundle) *StorageView {
	st := storageOf(b)
	v := &StorageView{Kind: string(st.Kind), SizeMB: st.SizeMB, Overwrite: st.Overwrite, NASURL: st.NASURL, NASUsername: st.NASUsername,
		MaxSDMB: b.doc.MaxSDMB(), NASProtocols: b.doc.NASProtocols(), Status: s.storageStatus(ctx, b), Records: recordViews(b.doc)}
	if v.NASProtocols == nil {
		v.NASProtocols = []string{}
	}
	if b.storage != nil {
		v.HasNASPassword = len(b.storage.NasSecretEnc) > 0
	}
	if st.Kind == domain.StorageNAS {
		s.storage.mu.Lock()
		v.NASError = s.storage.nas[b.cam.ID].Error
		s.storage.mu.Unlock()
	}
	return v
}

// GetStorage returns where a camera records and the state of its card or
// share.
func (s *Service) GetStorage(ctx context.Context, id string) (*StorageView, error) {
	b, err := s.loadBundle(ctx, id)
	if err != nil {
		return nil, err
	}
	return s.storageView(ctx, b), nil
}

// UpdateStorage gives a camera an SD card, a NAS share or neither. A card
// that grows must fit on the node's disk (D91); taking the card out wipes
// it, and a smaller one keeps the newest recordings that fit.
func (s *Service) UpdateStorage(ctx context.Context, actor Actor, id string, in StorageInput) (*StorageView, error) {
	s.storage.write.Lock()
	defer s.storage.write.Unlock()
	b, err := s.loadBundle(ctx, id)
	if err != nil {
		return nil, err
	}
	cur := storageOf(b)
	next := cur
	if in.Kind != nil {
		next.Kind = domain.StorageKind(strings.TrimSpace(*in.Kind))
	}
	if in.SizeMB != nil {
		next.SizeMB = *in.SizeMB
	}
	if in.Overwrite != nil {
		next.Overwrite = *in.Overwrite
	}
	if in.NASURL != nil {
		next.NASURL = strings.TrimSpace(*in.NASURL)
	}
	if in.NASUsername != nil {
		next.NASUsername = strings.TrimSpace(*in.NASUsername)
	}
	maxSD := b.doc.MaxSDMB()
	if next.Kind == domain.StorageSD && next.SizeMB == 0 {
		next.SizeMB = min(domain.DefaultSDSize, maxSD)
	}
	if err := domain.ValidateStorage(next, maxSD, b.doc.NASProtocols()); err != nil {
		return nil, err
	}
	if len(next.NASUsername) > 128 {
		return nil, domain.Invalid("nas_username", "must be at most 128 characters")
	}
	var secretEnc []byte
	if b.storage != nil {
		secretEnc = b.storage.NasSecretEnc
	}
	if in.NASPassword != nil {
		switch pw := *in.NASPassword; {
		case pw == "":
			secretEnc = nil
		case len(pw) > 256:
			return nil, domain.Invalid("nas_password", "must be at most 256 characters")
		default:
			secretEnc = s.box.Seal([]byte(pw), "camera_storage:"+id)
		}
	}
	if next.Kind == domain.StorageSD && (cur.Kind != domain.StorageSD || next.SizeMB > cur.SizeMB) {
		if err := s.admitDisk(ctx, id, next.SizeMB); err != nil {
			return nil, err
		}
	}
	if next.Kind == domain.StorageSD {
		if err := s.ensureSDDir(id); err != nil {
			return nil, err
		}
	}
	err = s.store.W().UpsertCameraStorage(ctx, db.UpsertCameraStorageParams{CameraID: id, Kind: string(next.Kind), SizeMb: int64(next.SizeMB),
		Overwrite: boolInt(next.Overwrite), NasUrl: next.NASURL, NasUsername: next.NASUsername, NasSecretEnc: secretEnc})
	if err != nil {
		return nil, err
	}
	switch {
	case cur.Kind == domain.StorageSD && next.Kind != domain.StorageSD:
		s.wipeSD(ctx, id)
	case next.Kind == domain.StorageSD && next.SizeMB < cur.SizeMB:
		s.pruneSD(ctx, id, int64(next.SizeMB)<<20)
	}
	if cur.Kind == domain.StorageNAS && (next.Kind != domain.StorageNAS || next.NASURL != cur.NASURL) {
		// The recordings stay on the share; the camera no longer knows them.
		if _, err := s.store.W().DeleteCameraRecordings(ctx, db.DeleteCameraRecordingsParams{CameraID: id, Location: string(domain.StorageNAS)}); err != nil {
			s.log.Warn("cannot forget the NAS recordings", "camera", id, "error", err)
		}
	}
	s.storage.forget(id)
	if next.Kind != domain.StorageSD {
		s.endSDFaults(ctx, id)
	}
	s.audit(ctx, actor, "storage.update", "camera", id, map[string]any{"kind": next.Kind, "size_mb": next.SizeMB, "overwrite": next.Overwrite,
		"nas_url": next.NASURL, "nas_username": next.NASUsername, "nas_password_changed": in.NASPassword != nil})
	if cur.Kind == domain.StorageNAS || next.Kind == domain.StorageNAS {
		// The camera connects to its share at once: its firewall lets it
		// first.
		s.refreshFirewalls(ctx, firewallRefresh)
	}
	s.pushStorage(ctx, id)
	return s.GetStorage(ctx, id)
}

func boolInt(v bool) int64 {
	if v {
		return 1
	}
	return 0
}

// endSDFaults ends the SD faults of a camera that no longer has a card.
func (s *Service) endSDFaults(ctx context.Context, id string) {
	open, err := s.openFaults(ctx, id)
	if err != nil {
		return
	}
	for _, f := range open {
		if _, sd := domain.SDFaultStates[f.Kind]; sd {
			if err := s.endFault(ctx, f, "storage changed"); err != nil {
				s.log.Warn("cannot end an SD fault", "camera", id, "fault", f.ID, "error", err)
			}
		}
	}
}

// admitDisk checks that a card of sizeMB for the camera fits on the
// node's disk, with what the other cards promised and have not used yet.
func (s *Service) admitDisk(ctx context.Context, id string, sizeMB int) error {
	free, ok := s.diskFree(s.opts.DataDir)
	if !ok {
		return nil
	}
	rows, err := s.store.R().PromisedSD(ctx)
	if err != nil {
		return err
	}
	var promised, usedHere uint64
	for _, r := range rows {
		if r.CameraID == id {
			usedHere = uint64(max(r.Used, 0))
			continue
		}
		if left := r.SizeMb<<20 - r.Used; left > 0 {
			promised += uint64(left)
		}
	}
	// What this camera already wrote is off the free space, and part of
	// its new card.
	return domain.AdmitDisk(free+usedHere, promised, sizeMB)
}

// wipeSD deletes every recording on the camera's card. The directory
// stays: a running camera can only read the one it was confined with.
func (s *Service) wipeSD(ctx context.Context, id string) {
	if _, err := s.store.W().DeleteCameraRecordings(ctx, db.DeleteCameraRecordingsParams{CameraID: id, Location: string(domain.StorageSD)}); err != nil {
		s.log.Warn("cannot forget the SD recordings", "camera", id, "error", err)
	}
	entries, err := os.ReadDir(s.sdDir(id))
	if err != nil {
		return
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(s.sdDir(id), e.Name())); err != nil {
			s.log.Warn("cannot wipe the SD card", "camera", id, "error", err)
		}
	}
}

// pruneSD deletes the oldest recordings on the card until what stays
// takes at most capacity bytes.
func (s *Service) pruneSD(ctx context.Context, id string, capacity int64) {
	loc := string(domain.StorageSD)
	for {
		u, err := s.store.R().RecordingUsage(ctx, db.RecordingUsageParams{CameraID: id, Location: loc})
		if err != nil || u.Used <= capacity {
			return
		}
		old, err := s.store.R().OldestRecordings(ctx, db.OldestRecordingsParams{CameraID: id, Location: loc, Limit: 20})
		if err != nil || len(old) == 0 {
			return
		}
		freed := int64(0)
		for _, r := range old {
			if u.Used-freed <= capacity {
				break
			}
			s.deleteSDFile(ctx, id, r)
			freed += r.Size
		}
	}
}

func (s *Service) deleteSDFile(ctx context.Context, id string, r db.Recording) {
	if err := s.store.W().DeleteRecording(ctx, r.ID); err != nil {
		s.log.Warn("cannot delete a recording", "camera", id, "error", err)
		return
	}
	if p, ok := s.sdPath(id, r.Name); ok {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			s.log.Warn("cannot delete a recording", "camera", id, "error", err)
		}
		// Day directories go once empty.
		_ = os.Remove(filepath.Dir(p))
	}
}

// sdPath is where a recording lives on the node, for a name that stays
// inside the card.
func (s *Service) sdPath(id, name string) (string, bool) {
	if !domain.ValidRecordingName(name) {
		return "", false
	}
	return filepath.Join(s.sdDir(id), filepath.FromSlash(name)), true
}

// FormatStorage wipes a camera's SD card, as the device's format does.
func (s *Service) FormatStorage(ctx context.Context, actor Actor, id string) (*StorageView, error) {
	s.storage.write.Lock()
	defer s.storage.write.Unlock()
	b, err := s.loadBundle(ctx, id)
	if err != nil {
		return nil, err
	}
	if b.storageKind() != domain.StorageSD {
		return nil, domain.Conflict("", "the camera has no SD card")
	}
	switch s.sdFault(ctx, id) {
	case domain.SDAbsent:
		return nil, domain.Conflict("", "there is no card in the slot")
	case domain.SDReadOnly:
		return nil, domain.Conflict("", "the card is read only")
	}
	s.wipeSD(ctx, id)
	s.storage.setFull(id, false)
	s.audit(ctx, actor, "storage.format", "camera", id, nil)
	s.pushStorage(ctx, id)
	return s.GetStorage(ctx, id)
}

func recordingView(r db.Recording) RecordingView {
	return RecordingView{ID: r.ID, Name: r.Name, Kind: r.Kind, Stream: r.Stream, EventID: r.EventID, EventType: r.EventType, Size: r.Size,
		Start: store.Time(r.StartAt), End: store.Time(r.EndAt), Location: r.Location}
}

// ListRecordings returns the newest recordings of the camera's current
// card or share, of a kind when given.
func (s *Service) ListRecordings(ctx context.Context, id, kind string) ([]RecordingView, error) {
	b, err := s.loadBundle(ctx, id)
	if err != nil {
		return nil, err
	}
	out := []RecordingView{}
	loc := b.storageKind()
	if loc == domain.StorageNone {
		return out, nil
	}
	if kind != "" && kind != engine.FileSnapshot && kind != engine.FileClip {
		return nil, domain.Invalid("kind", "must be snapshot or clip")
	}
	rows, err := s.store.R().RecentRecordings(ctx, db.RecentRecordingsParams{CameraID: id, Location: string(loc), Kind: kind, Limit: recentRecordings})
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		out = append(out, recordingView(r))
	}
	return out, nil
}

// OpenRecording reads a recording for the panel: from the node's disk for
// the SD card, through the camera for its share.
func (s *Service) OpenRecording(ctx context.Context, id, recID string) (*RecordingView, io.ReadCloser, error) {
	r, err := s.store.R().GetRecording(ctx, recID)
	if err != nil || r.CameraID != id {
		return nil, nil, domain.ErrNotFound
	}
	v := recordingView(r)
	switch domain.StorageKind(r.Location) {
	case domain.StorageSD:
		p, ok := s.sdPath(id, r.Name)
		if !ok {
			return nil, nil, domain.ErrNotFound
		}
		f, err := os.Open(p)
		if err != nil {
			return nil, nil, domain.ErrNotFound
		}
		return &v, f, nil
	default:
		ss := s.session(id)
		if ss == nil || !ss.active() {
			return nil, nil, domain.Conflict("", "start the camera to read the recordings on its share")
		}
		return &v, &nasReader{ctx: ctx, conn: ss.conn(), name: r.Name}, nil
	}
}

// nasReader reads a recording on a camera's share through the camera, in
// the largest pieces a message carries.
type nasReader struct {
	ctx  context.Context
	conn *ipc.Conn
	name string
	off  int64
	eof  bool
	buf  []byte
}

func (n *nasReader) Read(p []byte) (int, error) {
	if len(n.buf) == 0 {
		if n.eof {
			return 0, io.EOF
		}
		var out ipc.FileData
		if err := n.conn.Request(n.ctx, ipc.TypeFileRead, ipc.FileRead{Name: n.name, Offset: n.off, Length: ipc.MaxFileRead}, &out); err != nil {
			return 0, err
		}
		n.buf, n.eof = out.Data, out.EOF
		n.off += int64(len(out.Data))
		if len(n.buf) == 0 {
			if n.eof {
				return 0, io.EOF
			}
			return 0, io.ErrUnexpectedEOF
		}
	}
	k := copy(p, n.buf)
	n.buf = n.buf[k:]
	return k, nil
}

func (n *nasReader) Close() error { return nil }

// recordEventFiles stores what an event records on the camera's SD card;
// the camera records to its share itself.
func (s *Service) recordEventFiles(b *cameraBundle, e engine.Event) {
	spec := b.doc.Events[e.Type].Record
	if spec == nil || b.storageKind() != domain.StorageSD {
		return
	}
	rec := *spec
	s.goBackground(func(ctx context.Context) { s.recordToSD(ctx, b.cam.ID, e, rec) })
}

// recordToSD writes an event's snapshot and clip to the camera's card, as
// its state allows: a card out, failing or read only records nothing, a
// full one overwrites the oldest recordings or refuses.
func (s *Service) recordToSD(ctx context.Context, id string, e engine.Event, rec profile.RecordSpec) {
	s.storage.write.Lock()
	defer s.storage.write.Unlock()
	b, err := s.loadBundle(ctx, id)
	if err != nil || b.storageKind() != domain.StorageSD {
		return
	}
	// A card out, failing, read only or full records nothing.
	status := s.storageStatus(ctx, b)
	if status.State != engine.StoragePresent {
		s.log.Debug("the SD card does not record", "camera", id, "state", status.State)
		return
	}
	stream := rec.Stream
	if stream == "" {
		stream = "main"
	}
	changed := false
	if rec.Snapshot {
		if data, err := s.recordingSnapshot(ctx, b, stream); err != nil {
			s.log.Warn("cannot record the event's snapshot", "camera", id, "event", e.ID, "error", err)
		} else if s.storeSD(ctx, b, e, engine.FileSnapshot, stream, data, 0) {
			changed = true
		}
	}
	if d := rec.Clip.D(); d > 0 {
		if data, err := s.recordingClip(ctx, b, stream, d); err != nil {
			s.log.Warn("cannot record the event's clip", "camera", id, "event", e.ID, "error", err)
		} else if s.storeSD(ctx, b, e, engine.FileClip, stream, data, d) {
			changed = true
		}
	}
	if changed || s.storageStatus(ctx, b).State != status.State {
		s.pushStorage(ctx, id)
	}
}

// storeSD writes one recording to the card; it reports whether the card
// changed.
func (s *Service) storeSD(ctx context.Context, b *cameraBundle, e engine.Event, kind, stream string, data []byte, d time.Duration) bool {
	id := b.cam.ID
	st := storageOf(b)
	capacity := int64(st.SizeMB) << 20
	n := int64(len(data))
	if n > capacity {
		s.log.Warn("a recording is larger than the SD card", "camera", id, "bytes", n)
		return false
	}
	loc := string(domain.StorageSD)
	u, err := s.store.R().RecordingUsage(ctx, db.RecordingUsageParams{CameraID: id, Location: loc})
	if err != nil {
		return false
	}
	changed := false
	if u.Used+n > capacity {
		if !st.Overwrite {
			s.storage.setFull(id, true)
			s.log.Info("the SD card is full", "camera", id)
			return false
		}
		s.pruneSD(ctx, id, capacity-n)
		changed = true
	}
	ext := "jpg"
	if kind == engine.FileClip {
		ext = "ts"
	}
	name := domain.RecordingName(e.Type, e.ID, e.At, ext)
	p, _ := s.sdPath(id, name)
	if err := writeFileAtomic(p, data); err != nil {
		s.log.Warn("cannot write to the SD card", "camera", id, "error", err)
		return changed
	}
	err = s.store.W().InsertRecording(ctx, db.InsertRecordingParams{ID: ulid.Make().String(), CameraID: id, EventID: e.ID, EventType: e.Type,
		Kind: kind, Stream: stream, Name: name, Size: n, StartAt: e.At.UnixMilli(), EndAt: e.At.Add(d).UnixMilli(), Location: loc,
		CreatedAt: time.Now().UnixMilli()})
	if err != nil {
		_ = os.Remove(p)
		s.log.Warn("cannot index a recording", "camera", id, "error", err)
		return changed
	}
	return true
}

// writeFileAtomic writes a file the camera may read, whole or not at all.
func writeFileAtomic(p string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	// The day's directory, whatever the service's umask.
	if err := os.Chmod(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".rec-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), p)
}

// streamFiles are the encoded files of a camera stream.
func (s *Service) streamFiles(ctx context.Context, b *cameraBundle, stream string) (media.Files, db.Rendition, string, error) {
	for _, st := range b.streams {
		if st.Stream != stream || !st.RenditionID.Valid {
			continue
		}
		r, _, key, err := s.renditionInfo(ctx, st.RenditionID.String)
		if err != nil {
			return media.Files{}, r, "", err
		}
		if !s.lib.Ready(key, r.Codec) {
			return media.Files{}, r, "", errors.New("the stream is not encoded yet")
		}
		return s.lib.RenditionFiles(key, r.Codec), r, key, nil
	}
	return media.Files{}, db.Rendition{}, "", fmt.Errorf("the camera has no %s stream", stream)
}

func (s *Service) recordingSnapshot(ctx context.Context, b *cameraBundle, stream string) ([]byte, error) {
	files, _, _, err := s.streamFiles(ctx, b, stream)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(files.Snapshot)
}

func (s *Service) recordingClip(ctx context.Context, b *cameraBundle, stream string, d time.Duration) ([]byte, error) {
	files, r, key, err := s.streamFiles(ctx, b, stream)
	if err != nil {
		return nil, err
	}
	s.storage.mu.Lock()
	src := s.storage.sources[key]
	s.storage.mu.Unlock()
	if src == nil {
		data, err := os.ReadFile(files.Stream)
		if err != nil {
			return nil, err
		}
		if src, err = media.ParseStream(r.Codec, data); err != nil {
			return nil, err
		}
		src.Info = engine.StreamInfo{Name: stream, Codec: r.Codec, Width: int(r.Width), Height: int(r.Height), FPS: int(r.Fps), GOP: int(r.Gop), Bitrate: int(r.Bitrate)}
		s.storage.mu.Lock()
		if len(s.storage.sources) >= maxSources {
			clear(s.storage.sources)
		}
		s.storage.sources[key] = src
		s.storage.mu.Unlock()
	}
	return media.Clip(src, d)
}

// findFiles answers a camera's search of its recordings.
func (s *Service) findFiles(ctx context.Context, id string, q ipc.FilesFind) (ipc.FilesFound, error) {
	b, err := s.loadBundle(ctx, id)
	if err != nil {
		return ipc.FilesFound{}, err
	}
	out := ipc.FilesFound{Files: []engine.FileInfo{}}
	loc := string(b.storageKind())
	if b.storageKind() == domain.StorageNone {
		return out, nil
	}
	if q.Name != "" {
		r, err := s.store.R().GetRecordingByName(ctx, db.GetRecordingByNameParams{CameraID: id, Location: loc, Name: q.Name})
		if err == nil {
			out.Files = append(out.Files, fileInfo(r))
		}
		return out, nil
	}
	from, to := int64(0), int64(1)<<62
	if !q.From.IsZero() {
		from = q.From.UnixMilli()
	}
	if !q.To.IsZero() {
		to = q.To.UnixMilli()
	}
	limit := q.Limit
	if limit <= 0 || limit > maxFilesFound {
		limit = maxFilesFound
	}
	rows, err := s.store.R().FindRecordings(ctx, db.FindRecordingsParams{CameraID: id, Location: loc, FromAt: from, ToAt: to, Kind: q.Kind,
		EventType: q.Event, Limit: int64(limit)})
	if err != nil {
		return out, err
	}
	for _, r := range rows {
		out.Files = append(out.Files, fileInfo(r))
	}
	return out, nil
}

func fileInfo(r db.Recording) engine.FileInfo {
	return engine.FileInfo{Name: r.Name, Kind: r.Kind, Event: r.EventType, Stream: r.Stream, Size: r.Size, Start: store.Time(r.StartAt), End: store.Time(r.EndAt)}
}

// indexNAS records what the camera wrote to its share for one of its
// events.
func (s *Service) indexNAS(ctx context.Context, id string, rec ipc.Recorded) {
	b, err := s.loadBundle(ctx, id)
	if err != nil || b.storageKind() != domain.StorageNAS {
		return
	}
	f := rec.File
	ev, err := s.store.R().GetEvent(ctx, rec.EventID)
	switch {
	case err != nil || ev.CameraID != id:
		s.log.Warn("camera reported a recording of an event that is not its own", "camera", id, "event", truncate(rec.EventID, 64))
		return
	case f.Kind != engine.FileSnapshot && f.Kind != engine.FileClip,
		!domain.ValidRecordingName(f.Name), f.Size < 0, f.Size > maxRecordingSize,
		f.End.Before(f.Start), f.End.Sub(f.Start) > media.MaxClip:
		s.log.Warn("camera reported an invalid recording", "camera", id, "name", truncate(f.Name, 64))
		return
	}
	err = s.store.W().InsertRecording(ctx, db.InsertRecordingParams{ID: ulid.Make().String(), CameraID: id, EventID: ev.ID, EventType: ev.Type,
		Kind: f.Kind, Stream: truncate(f.Stream, 32), Name: f.Name, Size: f.Size, StartAt: f.Start.UnixMilli(), EndAt: f.End.UnixMilli(),
		Location: string(domain.StorageNAS), CreatedAt: time.Now().UnixMilli()})
	if err != nil && !store.IsUnique(err) {
		s.log.Warn("cannot index a NAS recording", "camera", id, "error", err)
		return
	}
	s.pub.Publish("cameras", "storage", map[string]any{"camera_id": id})
}

// setNASState keeps what a camera says of its share.
func (s *Service) setNASState(id string, st ipc.NASState) {
	st.Error = truncate(st.Error, 300)
	s.storage.mu.Lock()
	prev, known := s.storage.nas[id]
	s.storage.nas[id] = st
	s.storage.mu.Unlock()
	if !known || prev != st {
		s.pub.Publish("cameras", "storage", map[string]any{"camera_id": id})
	}
}

// nasHost is the host of a share's URL.
func nasHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Hostname()
}
