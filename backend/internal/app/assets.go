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
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/corticoide/mockvision/backend/internal/domain"
	"github.com/corticoide/mockvision/backend/internal/ipc"
	"github.com/corticoide/mockvision/backend/internal/media"
	"github.com/corticoide/mockvision/backend/internal/profile"
	"github.com/corticoide/mockvision/backend/internal/store"
	"github.com/corticoide/mockvision/backend/internal/store/db"
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
	ext := ".jpg"
	if a.Mime == "image/png" {
		ext = ".png"
	}
	return s.lib.AssetPath(a.Sha256, ext), a.Mime, nil
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
	if _, err := s.store.W().DeleteAsset(ctx, id); err != nil {
		if store.IsForeignKey(err) {
			return domain.Conflict("", "the asset is used by cameras; change their image first")
		}
		return err
	}
	ext := ".jpg"
	if a.Mime == "image/png" {
		ext = ".png"
	}
	_ = os.Remove(s.lib.AssetPath(a.Sha256, ext))
	s.audit(ctx, actor, "asset.delete", "asset", id, nil)
	return nil
}

// ensureRenditionRow returns the rendition of an asset for the settings,
// creating it as pending when needed.
func (s *Service) ensureRenditionRow(ctx context.Context, asset db.Asset, set profile.StreamSettings) (db.Rendition, error) {
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

type encodeJob struct {
	done  chan struct{}
	files media.Files
	err   error
}

func renditionParams(r db.Rendition) media.Params {
	return media.Params{Codec: r.Codec, Width: int(r.Width), Height: int(r.Height), FPS: int(r.Fps), GOP: int(r.Gop), Bitrate: int(r.Bitrate)}
}

// encodeRendition makes sure a rendition is encoded, running FFmpeg once
// even when several cameras need it at the same time.
func (s *Service) encodeRendition(ctx context.Context, id string) (media.Files, error) {
	r, err := s.store.R().GetRendition(ctx, id)
	if err != nil {
		return media.Files{}, store.NotFound(err)
	}
	asset, err := s.store.R().GetAsset(ctx, r.AssetID)
	if err != nil {
		return media.Files{}, err
	}
	params := renditionParams(r)
	key := params.Key(asset.Sha256)
	if s.lib.Ready(key) {
		if r.Status != string(domain.RenditionReady) {
			_ = s.store.W().SetRenditionStatus(ctx, db.SetRenditionStatusParams{ID: id, Status: string(domain.RenditionReady), Sha256: key})
		}
		return s.lib.RenditionFiles(key), nil
	}
	s.mu.Lock()
	job, running := s.encodes[key]
	if !running {
		job = &encodeJob{done: make(chan struct{})}
		s.encodes[key] = job
	}
	s.mu.Unlock()
	if !running {
		go func() {
			defer func() {
				s.mu.Lock()
				delete(s.encodes, key)
				s.mu.Unlock()
				close(job.done)
			}()
			ext := ".jpg"
			if asset.Mime == "image/png" {
				ext = ".png"
			}
			start := time.Now()
			job.files, job.err = s.lib.Encode(s.baseCtx, s.lib.AssetPath(asset.Sha256, ext), params, key)
			st := domain.RenditionReady
			msg := ""
			if job.err != nil {
				st, msg = domain.RenditionFailed, job.err.Error()
				s.log.Error("rendition failed", "rendition", id, "error", job.err)
			} else {
				s.log.Info("rendition encoded", "rendition", id, "size", fmt.Sprintf("%dx%d", params.Width, params.Height), "took", time.Since(start).Round(time.Millisecond))
			}
			_ = s.store.W().SetRenditionStatus(context.Background(), db.SetRenditionStatusParams{ID: id, Status: string(st), Sha256: key, Error: msg})
		}()
	}
	select {
	case <-job.done:
		return job.files, job.err
	case <-ctx.Done():
		return media.Files{}, ctx.Err()
	}
}

// encodeAsync starts encoding a rendition in the background.
func (s *Service) encodeAsync(id string) {
	go func() {
		ctx, cancel := context.WithTimeout(s.baseCtx, 10*time.Minute)
		defer cancel()
		_, _ = s.encodeRendition(ctx, id)
	}()
}

// prepareStreams computes each stream's settings from the camera state,
// encodes what is missing and returns the streams for the camera process.
func (s *Service) prepareStreams(ctx context.Context, b *cameraBundle) ([]ipc.Stream, error) {
	values := b.values()
	var out []ipc.Stream
	for _, st := range b.streams {
		set, err := b.model.StreamFor(st.Stream, values)
		if err != nil {
			return nil, err
		}
		asset, err := s.store.R().GetAsset(ctx, st.AssetID)
		if err != nil {
			return nil, fmt.Errorf("asset %s: %w", st.AssetID, err)
		}
		rend, err := s.ensureRenditionRow(ctx, asset, set)
		if err != nil {
			return nil, err
		}
		if !st.RenditionID.Valid || st.RenditionID.String != rend.ID {
			if err := s.store.W().UpsertCameraStream(ctx, db.UpsertCameraStreamParams{CameraID: b.cam.ID, Stream: st.Stream, AssetID: asset.ID, RenditionID: store.NullString(rend.ID)}); err != nil {
				return nil, err
			}
		}
		files, err := s.encodeRendition(ctx, rend.ID)
		if err != nil {
			return nil, err
		}
		out = append(out, ipc.Stream{
			Name: st.Stream, Codec: set.Codec, Width: set.Width, Height: set.Height, FPS: set.FPS, GOP: set.GOP,
			Bitrate: set.Bitrate, GOPPath: files.GOP, SnapshotPath: files.Snapshot,
		})
	}
	if len(out) == 0 {
		return nil, errors.New("the camera has no stream")
	}
	return out, nil
}

// regenerateStreams re-encodes streams after an effective parameter
// changed and hands them to the running camera.
func (s *Service) regenerateStreams(id string) {
	ctx, cancel := context.WithTimeout(s.baseCtx, 10*time.Minute)
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

// SnapshotFile returns the JPEG of a camera's main stream, the preview of
// the panel (D46: snapshots in v1).
func (s *Service) SnapshotFile(ctx context.Context, id string) (string, error) {
	b, err := s.loadBundle(ctx, id)
	if err != nil {
		return "", err
	}
	for _, st := range b.streams {
		if st.Stream != "main" || !st.RenditionID.Valid {
			continue
		}
		r, err := s.store.R().GetRendition(ctx, st.RenditionID.String)
		if err != nil || r.Status != string(domain.RenditionReady) {
			break
		}
		return s.lib.RenditionFiles(r.Sha256).Snapshot, nil
	}
	return "", domain.Conflict("", "the stream is not encoded yet")
}
