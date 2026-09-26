package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
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
	"github.com/corticoide/mockvision/backend/internal/worker"
)

const builtinAssetName = "MockVision test pattern"

// UploadAsset stores an image; the same content is stored once.
func (s *Service) UploadAsset(ctx context.Context, actor Actor, filename string, r io.Reader) (*AssetView, error) {
	data, err := io.ReadAll(io.LimitReader(r, media.MaxImageBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > media.MaxImageBytes {
		return nil, domain.Invalid("file", "images are limited to %d MB", media.MaxImageBytes>>20)
	}
	info, err := media.ProbeImage(bytes.NewReader(data))
	if err != nil {
		return nil, domain.Invalid("file", "%v", err)
	}
	a, err := s.storeAsset(ctx, data, info, cleanFilename(filename), false)
	if err != nil {
		return nil, err
	}
	s.audit(ctx, actor, "asset.upload", "asset", a.ID, map[string]any{"filename": a.Filename})
	return a, nil
}

func cleanFilename(name string) string {
	name = filepath.Base(strings.ReplaceAll(name, "\\", "/"))
	if name == "." || name == "/" || name == "" {
		name = "image"
	}
	if len(name) > 120 {
		name = name[:120]
	}
	return name
}

func (s *Service) storeAsset(ctx context.Context, data []byte, info media.ImageInfo, filename string, builtin bool) (*AssetView, error) {
	sum := sha256.Sum256(data)
	sha := hex.EncodeToString(sum[:])
	if existing, err := s.store.R().GetAssetBySHA256(ctx, sha); err == nil {
		v := assetView(existing, 0)
		return &v, nil
	}
	path := s.lib.AssetPath(sha, info.Ext)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return nil, err
	}
	if err := os.Rename(tmp, path); err != nil {
		return nil, err
	}
	row := db.Asset{
		ID: ulid.Make().String(), Sha256: sha, Kind: "image", Mime: info.MIME, Width: int64(info.Width), Height: int64(info.Height),
		Size: int64(len(data)), Filename: filename, Builtin: store.Int(builtin), CreatedAt: time.Now().UnixMilli(),
	}
	err := s.store.W().InsertAsset(ctx, db.InsertAssetParams{
		ID: row.ID, Sha256: row.Sha256, Kind: row.Kind, Mime: row.Mime, Width: row.Width, Height: row.Height,
		Size: row.Size, Filename: row.Filename, Builtin: row.Builtin, CreatedAt: row.CreatedAt,
	})
	if err != nil {
		if store.IsUnique(err) {
			if existing, err := s.store.R().GetAssetBySHA256(ctx, sha); err == nil {
				v := assetView(existing, 0)
				return &v, nil
			}
		}
		return nil, err
	}
	v := assetView(row, 0)
	return &v, nil
}

func assetView(a db.Asset, cameras int64) AssetView {
	return AssetView{ID: a.ID, Filename: a.Filename, MIME: a.Mime, Width: int(a.Width), Height: int(a.Height), Size: a.Size,
		SHA256: a.Sha256, Builtin: store.Bool(a.Builtin), CameraCount: int(cameras), CreatedAt: store.Time(a.CreatedAt)}
}

// ensureBuiltinAsset creates the test pattern used when a camera has no
// uploaded image.
func (s *Service) ensureBuiltinAsset(ctx context.Context) (*AssetView, error) {
	rows, err := s.store.R().ListAssets(ctx)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		if store.Bool(r.Builtin) {
			v := AssetView{ID: r.ID, Filename: r.Filename, Builtin: true}
			return &v, nil
		}
	}
	tmp := filepath.Join(s.lib.AssetsDir, "builtin.tmp.jpg")
	if err := s.lib.TestPattern(ctx, tmp); err != nil {
		s.log.Warn("cannot create the default image", "error", err)
		return nil, err
	}
	defer os.Remove(tmp)
	data, err := os.ReadFile(tmp)
	if err != nil {
		return nil, err
	}
	info, err := media.ProbeImage(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	return s.storeAsset(ctx, data, info, builtinAssetName, true)
}

// ListAssets returns every asset.
func (s *Service) ListAssets(ctx context.Context) ([]AssetView, error) {
	rows, err := s.store.R().ListAssets(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]AssetView, 0, len(rows))
	for _, r := range rows {
		out = append(out, assetView(db.Asset{ID: r.ID, Sha256: r.Sha256, Kind: r.Kind, Mime: r.Mime, Width: r.Width, Height: r.Height,
			Size: r.Size, Filename: r.Filename, Builtin: r.Builtin, CreatedAt: r.CreatedAt}, r.CameraCount))
	}
	return out, nil
}

// AssetFile returns the path and type of an asset's content.
func (s *Service) AssetFile(ctx context.Context, id string) (string, string, error) {
	a, err := s.store.R().GetAsset(ctx, id)
	if err != nil {
		return "", "", store.NotFound(err)
	}
	return s.lib.AssetPath(a.Sha256, assetExt(a)), a.Mime, nil
}

// DeleteAsset removes an unused asset (RN-12).
func (s *Service) DeleteAsset(ctx context.Context, actor Actor, id string) error {
	a, err := s.store.R().GetAsset(ctx, id)
	if err != nil {
		return store.NotFound(err)
	}
	if store.Bool(a.Builtin) {
		return domain.Conflict("", "the built-in test pattern cannot be deleted")
	}
	// Its renditions go with it: the rows by cascade, the files here.
	var dirs []string
	if rows, err := s.store.R().ListRenditionsWithAsset(ctx); err == nil {
		for _, r := range rows {
			if r.AssetID == id {
				key := renditionParams(db.Rendition{Codec: r.Codec, Width: r.Width, Height: r.Height, Fps: r.Fps, Gop: r.Gop, Bitrate: r.Bitrate}).Key(r.AssetSha256)
				dirs = append(dirs, s.lib.RenditionFiles(key, r.Codec).Dir)
			}
		}
	}
	if _, err := s.store.W().DeleteAsset(ctx, id); err != nil {
		if store.IsForeignKey(err) {
			return domain.Conflict("", "the asset is used by cameras; change their image first")
		}
		return err
	}
	_ = os.Remove(s.lib.AssetPath(a.Sha256, assetExt(a)))
	for _, d := range dirs {
		_ = os.RemoveAll(d)
	}
	s.audit(ctx, actor, "asset.delete", "asset", id, map[string]string{"name": a.Filename})
	return nil
}

// ensureRenditionRow returns the rendition of an asset for the settings,
// creating it as pending when needed. Settings no encoder can honor, such
// as MJPEG above 2040 pixels, are refused here.
func (s *Service) ensureRenditionRow(ctx context.Context, asset db.Asset, set profile.StreamSettings) (db.Rendition, error) {
	mp := media.Params{Codec: set.Codec, Width: set.Width, Height: set.Height, FPS: set.FPS, GOP: set.GOP, Bitrate: set.Bitrate}
	if err := mp.Validate(); err != nil {
		return db.Rendition{}, domain.Invalid("stream", "%v", err)
	}
	p := db.GetRenditionByParamsParams{AssetID: asset.ID, Codec: set.Codec, Width: int64(set.Width), Height: int64(set.Height),
		Fps: int64(set.FPS), Gop: int64(set.GOP), Bitrate: int64(set.Bitrate)}
	if r, err := s.store.R().GetRenditionByParams(ctx, p); err == nil {
		return r, nil
	}
	err := s.store.W().InsertRendition(ctx, db.InsertRenditionParams{
		ID: ulid.Make().String(), AssetID: asset.ID, Codec: set.Codec, Width: p.Width, Height: p.Height,
		Fps: p.Fps, Gop: p.Gop, Bitrate: p.Bitrate, CreatedAt: time.Now().UnixMilli(),
	})
	if err != nil {
		return db.Rendition{}, err
	}
	return s.store.R().GetRenditionByParams(ctx, p)
}

// streamNames lists the streams a profile defines: main, then sub and
// third.
func streamNames(doc *profile.Document) []string {
	var out []string
	for _, name := range []string{"main", "sub", "third"} {
		if _, ok := doc.Media.Streams[name]; ok {
			out = append(out, name)
		}
	}
	return out
}

// streamError names the stream a validation error comes from.
func streamError(name string, err error) error {
	var verr *domain.ValidationError
	if errors.As(err, &verr) && len(verr.Fields) > 0 {
		return domain.Invalid(verr.Fields[0].Field, "stream %s: %s", name, verr.Fields[0].Message)
	}
	return err
}

// completeStreams gives a camera the streams of its profile it lacks, with
// the picture of its main stream: cameras created before sub and third
// streams were served only have a main one.
func (s *Service) completeStreams(ctx context.Context, b *cameraBundle) error {
	have := map[string]bool{}
	asset := ""
	for _, st := range b.streams {
		have[st.Stream] = true
		if asset == "" || st.Stream == "main" {
			asset = st.AssetID
		}
	}
	if asset == "" {
		return nil
	}
	added := false
	for _, name := range streamNames(b.doc) {
		if have[name] {
			continue
		}
		if err := s.store.W().UpsertCameraStream(ctx, db.UpsertCameraStreamParams{CameraID: b.cam.ID, Stream: name, AssetID: asset}); err != nil {
			return err
		}
		added = true
	}
	if !added {
		return nil
	}
	var err error
	b.streams, err = s.store.R().ListCameraStreams(ctx, b.cam.ID)
	return err
}

func renditionParams(r db.Rendition) media.Params {
	return media.Params{Codec: r.Codec, Width: int(r.Width), Height: int(r.Height), FPS: int(r.Fps), GOP: int(r.Gop), Bitrate: int(r.Bitrate)}
}

func assetExt(a db.Asset) string {
	if a.Mime == "image/png" {
		return ".png"
	}
	return ".jpg"
}

// encodeLock serializes the encoding of one rendition: a rendition job and
// a batch that prepares many may reach the same one. Callers release their
// use with dropEncodeLock, so the map does not grow with every rendition
// ever encoded.
func (s *Service) encodeLock(key string) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.encodes[key]
	if !ok {
		m = &refMutex{}
		s.encodes[key] = m
	}
	m.refs++
	return &m.Mutex
}

// dropEncodeLock releases a use of an encode lock taken with encodeLock.
func (s *Service) dropEncodeLock(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m, ok := s.encodes[key]; ok {
		if m.refs--; m.refs <= 0 {
			delete(s.encodes, key)
		}
	}
}

// refMutex is a mutex with the number of callers that hold or wait for it.
type refMutex struct {
	sync.Mutex
	refs int
}

// renditionInfo loads a rendition, its asset and its cache key.
func (s *Service) renditionInfo(ctx context.Context, id string) (db.Rendition, db.Asset, string, error) {
	r, err := s.store.R().GetRendition(ctx, id)
	if err != nil {
		return r, db.Asset{}, "", store.NotFound(err)
	}
	asset, err := s.store.R().GetAsset(ctx, r.AssetID)
	if err != nil {
		return r, asset, "", err
	}
	return r, asset, renditionParams(r).Key(asset.Sha256), nil
}

func renditionTitle(r db.Rendition, a db.Asset) string {
	return fmt.Sprintf("Encode %dx%d %s at %d fps from %s", r.Width, r.Height, codecLabel(r.Codec), r.Fps, a.Filename)
}

// codecLabel names a codec as cameras and clients show it.
func codecLabel(codec string) string {
	switch codec {
	case media.CodecH264:
		return "H.264"
	case media.CodecH265:
		return "H.265"
	case media.CodecMJPEG:
		return "MJPEG"
	}
	return codec
}

type renditionJobParams struct {
	RenditionID string `json:"rendition_id"`
}

// renditionJob queues the encoding of a rendition, or joins the job that
// is already on it: many cameras may need the same one at once.
func (s *Service) renditionJob(ctx context.Context, r db.Rendition, asset db.Asset, key string) (worker.Job, error) {
	j, _, err := s.jobs.Submit(ctx, worker.Spec{
		Type: JobRendition, Title: renditionTitle(r, asset), Params: renditionJobParams{RenditionID: r.ID},
		Key: "rendition:" + key, CreatedBy: "node",
	})
	return j, err
}

// encodeRendition makes sure a rendition is encoded and returns its files.
// The work runs as a job; ctx only bounds the wait.
func (s *Service) encodeRendition(ctx context.Context, id string) (media.Files, error) {
	r, asset, key, err := s.renditionInfo(ctx, id)
	if err != nil {
		return media.Files{}, err
	}
	if s.lib.Ready(key, r.Codec) {
		if r.Status != string(domain.RenditionReady) {
			_ = s.store.W().SetRenditionStatus(ctx, db.SetRenditionStatusParams{ID: id, Status: string(domain.RenditionReady), Sha256: key})
		}
		return s.lib.RenditionFiles(key, r.Codec), nil
	}
	j, err := s.renditionJob(ctx, r, asset, key)
	if err != nil {
		return media.Files{}, err
	}
	if j, err = s.jobs.Wait(ctx, j.ID); err != nil {
		return media.Files{}, err
	}
	if j.Status != worker.Completed {
		if j.Error != "" {
			return media.Files{}, errors.New(j.Error)
		}
		return media.Files{}, fmt.Errorf("the encoding job was %s", j.Status)
	}
	return s.lib.RenditionFiles(key, r.Codec), nil
}

// encodeAsync queues the encoding of a rendition without waiting for it.
func (s *Service) encodeAsync(id string) {
	r, asset, key, err := s.renditionInfo(s.baseCtx, id)
	if err != nil || s.lib.Ready(key, r.Codec) {
		return
	}
	if _, err := s.renditionJob(s.baseCtx, r, asset, key); err != nil {
		s.log.Warn("cannot queue a rendition", "rendition", id, "error", err)
	}
}

// runRendition is the rendition job: it encodes the stream, then the
// snapshot. The stream file on disk is its checkpoint, so a job resumed
// after a restart goes straight to the snapshot.
func (s *Service) runRendition(ctx context.Context, run *worker.Run) (any, error) {
	var p renditionJobParams
	if err := run.Params(&p); err != nil {
		return nil, err
	}
	r, asset, key, err := s.renditionInfo(ctx, p.RenditionID)
	if err != nil {
		return nil, err
	}
	start := time.Now()
	err = s.encodeSteps(ctx, run, r, asset, key, 0, 1)
	result := map[string]any{"rendition_id": r.ID, "resolution": fmt.Sprintf("%dx%d", r.Width, r.Height), "codec": r.Codec, "fps": r.Fps}
	switch {
	case err == nil:
		_ = s.store.W().SetRenditionStatus(ctx, db.SetRenditionStatusParams{ID: r.ID, Status: string(domain.RenditionReady), Sha256: key})
		s.log.Info("rendition encoded", "rendition", r.ID, "size", fmt.Sprintf("%dx%d", r.Width, r.Height), "took", time.Since(start).Round(time.Millisecond))
	case ctx.Err() == nil:
		// A stop or a cancel leaves the rendition pending for the next try.
		_ = s.store.W().SetRenditionStatus(ctx, db.SetRenditionStatusParams{ID: r.ID, Status: string(domain.RenditionFailed), Sha256: key, Error: err.Error()})
	}
	return result, err
}

// encodeSteps encodes what a rendition still lacks, reporting the job's
// progress between from and to.
func (s *Service) encodeSteps(ctx context.Context, run *worker.Run, r db.Rendition, asset db.Asset, key string, from, to float64) error {
	lock := s.encodeLock(key)
	defer s.dropEncodeLock(key)
	lock.Lock()
	defer lock.Unlock()
	if s.lib.Ready(key, r.Codec) {
		return nil
	}
	params := renditionParams(r)
	src := s.lib.AssetPath(asset.Sha256, assetExt(asset))
	span := to - from
	if !s.lib.StreamReady(key, r.Codec) {
		run.Step(fmt.Sprintf("Stream %dx%d", r.Width, r.Height), from)
		sctx, cancel := run.StepContext(ctx)
		err := s.lib.EncodeStream(sctx, src, params, key, func(frames int) {
			run.Progress(from + span*0.85*float64(frames)/float64(params.GOP))
		})
		cancel()
		if err != nil {
			return err
		}
	}
	run.Step("Snapshot", from+span*0.85)
	sctx, cancel := run.StepContext(ctx)
	defer cancel()
	return s.lib.EncodeSnapshot(sctx, src, params, key)
}

// prepareStreams computes each stream's settings from the camera state,
// encodes what is missing and returns the streams for the camera process.
func (s *Service) prepareStreams(ctx context.Context, b *cameraBundle) ([]ipc.Stream, error) {
	values := b.values()
	var out []ipc.Stream
	for _, st := range b.streams {
		set, rend, err := s.streamRendition(ctx, b, st, values)
		if err != nil {
			return nil, err
		}
		files, err := s.encodeRendition(ctx, rend.ID)
		if err != nil {
			return nil, err
		}
		out = append(out, ipc.Stream{
			Name: st.Stream, Codec: set.Codec, Width: set.Width, Height: set.Height, FPS: set.FPS, GOP: set.GOP,
			Bitrate: set.Bitrate, StreamPath: files.Stream, SnapshotPath: files.Snapshot,
		})
	}
	if len(out) == 0 {
		return nil, errors.New("the camera has no stream")
	}
	return out, nil
}

// streamRendition is the rendition a camera stream needs with the current
// parameter values; the stream is pointed at it.
func (s *Service) streamRendition(ctx context.Context, b *cameraBundle, st db.CameraStream, values map[string]any) (profile.StreamSettings, db.Rendition, error) {
	set, err := b.model.StreamFor(st.Stream, values)
	if err != nil {
		return set, db.Rendition{}, err
	}
	asset, err := s.store.R().GetAsset(ctx, st.AssetID)
	if err != nil {
		return set, db.Rendition{}, fmt.Errorf("asset %s: %w", st.AssetID, err)
	}
	rend, err := s.ensureRenditionRow(ctx, asset, set)
	if err != nil {
		return set, rend, err
	}
	if !st.RenditionID.Valid || st.RenditionID.String != rend.ID {
		if err := s.store.W().UpsertCameraStream(ctx, db.UpsertCameraStreamParams{CameraID: b.cam.ID, Stream: st.Stream, AssetID: asset.ID, RenditionID: store.NullString(rend.ID)}); err != nil {
			return set, rend, err
		}
	}
	return set, rend, nil
}

// regenerateStreams re-encodes streams after an effective parameter
// changed and hands them to the running camera.
func (s *Service) regenerateStreams(ctx context.Context, id string) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	b, err := s.loadBundle(ctx, id)
	if err != nil {
		return
	}
	streams, err := s.prepareStreams(ctx, b)
	if err != nil {
		s.log.Warn("cannot regenerate streams", "camera", id, "error", err)
		return
	}
	if ss := s.session(id); ss != nil && ss.active() {
		if err := ss.conn().Request(ctx, ipc.TypeReload, ipc.Reload{Streams: streams}, nil); err != nil {
			s.log.Warn("camera did not reload its streams", "camera", id, "error", err)
		}
	}
	if v, err := s.GetCamera(ctx, id); err == nil {
		s.pub.Publish("cameras", "updated", v)
	}
}

// regenDebounce lets a burst of encoder changes settle before a camera's
// streams are encoded again.
const regenDebounce = 500 * time.Millisecond

// regenState coalesces the re-encodings of one camera: at most one runs,
// and changes that arrive meanwhile lead to a single further run with the
// latest values. A client of the emulated API changing encoder settings in
// a loop can then keep at most one encoding per stream busy, instead of
// queuing one per value (audit A2).
type regenState struct{ again bool }

// regenerateStreamsLater re-encodes a camera's streams in the background.
func (s *Service) regenerateStreamsLater(id string) {
	s.mu.Lock()
	if st := s.regens[id]; st != nil {
		st.again = true
		s.mu.Unlock()
		return
	}
	st := &regenState{}
	s.regens[id] = st
	s.mu.Unlock()
	started := s.goBackground(func(ctx context.Context) {
		defer func() {
			s.mu.Lock()
			delete(s.regens, id)
			s.mu.Unlock()
		}()
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(regenDebounce):
			}
			s.mu.Lock()
			st.again = false
			s.mu.Unlock()
			s.regenerateStreams(ctx, id)
			s.mu.Lock()
			again := st.again
			s.mu.Unlock()
			if !again {
				return
			}
		}
	})
	if !started {
		s.mu.Lock()
		delete(s.regens, id)
		s.mu.Unlock()
	}
}

// renditionGrace is how long a rendition no camera uses is kept: a camera
// switching back to it soon finds it encoded.
const renditionGrace = 24 * time.Hour

// collectRenditions deletes renditions no camera stream uses, with their
// files, and the directories of renditions that no longer exist (deleted
// assets, older encoding recipes). Without it, every encoder setting ever
// used stayed on disk (audit A2).
func (s *Service) collectRenditions(ctx context.Context, now time.Time) {
	unused, err := s.store.R().ListUnusedRenditions(ctx, now.Add(-renditionGrace).UnixMilli())
	if err != nil {
		s.log.Warn("cannot list unused renditions", "error", err)
		return
	}
	removed := 0
	for _, r := range unused {
		key := renditionParams(db.Rendition{Codec: r.Codec, Width: r.Width, Height: r.Height, Fps: r.Fps, Gop: r.Gop, Bitrate: r.Bitrate}).Key(r.AssetSha256)
		lock := s.encodeLock(key)
		if !lock.TryLock() {
			continue // being encoded right now
		}
		if err := s.store.W().DeleteRendition(ctx, r.ID); err == nil {
			_ = os.RemoveAll(s.lib.RenditionFiles(key, r.Codec).Dir)
			removed++
		}
		lock.Unlock()
		s.dropEncodeLock(key)
	}
	if removed > 0 {
		s.log.Info("removed unused renditions", "count", removed)
	}
	s.sweepRenditionDirs(ctx, now)
}

// sweepRenditionDirs removes rendition directories that match no rendition,
// once they are an hour old so an encoding that just started is spared.
func (s *Service) sweepRenditionDirs(ctx context.Context, now time.Time) {
	rows, err := s.store.R().ListRenditionsWithAsset(ctx)
	if err != nil {
		return
	}
	known := make(map[string]bool, len(rows))
	for _, r := range rows {
		known[renditionParams(db.Rendition{Codec: r.Codec, Width: r.Width, Height: r.Height, Fps: r.Fps, Gop: r.Gop, Bitrate: r.Bitrate}).Key(r.AssetSha256)] = true
	}
	entries, err := os.ReadDir(s.lib.RenditionsDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() || known[e.Name()] || !validKey(e.Name()) {
			continue
		}
		if info, err := e.Info(); err != nil || now.Sub(info.ModTime()) < time.Hour {
			continue
		}
		_ = os.RemoveAll(filepath.Join(s.lib.RenditionsDir, e.Name()))
	}
}

// validKey reports whether a directory name is a rendition key: 64 hex
// digits, so nothing else in the directory is ever removed.
func validKey(name string) bool {
	if len(name) != 64 {
		return false
	}
	_, err := hex.DecodeString(name)
	return err == nil
}

// SnapshotFile returns the JPEG of a camera stream, the main one when
// stream is empty: the preview of the panel (D46: snapshots in v1).
func (s *Service) SnapshotFile(ctx context.Context, id, stream string) (string, error) {
	if stream == "" {
		stream = "main"
	}
	b, err := s.loadBundle(ctx, id)
	if err != nil {
		return "", err
	}
	for _, st := range b.streams {
		if st.Stream != stream {
			continue
		}
		if st.RenditionID.Valid {
			r, err := s.store.R().GetRendition(ctx, st.RenditionID.String)
			if err == nil && r.Status == string(domain.RenditionReady) {
				return s.lib.RenditionFiles(r.Sha256, r.Codec).Snapshot, nil
			}
		}
		return "", domain.Conflict("", "the stream is not encoded yet")
	}
	return "", fmt.Errorf("camera has no stream %q: %w", stream, domain.ErrNotFound)
}
