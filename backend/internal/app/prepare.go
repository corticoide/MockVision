package app

import (
	"context"
	"fmt"

	"github.com/corticoide/mockvision/backend/internal/domain"
	"github.com/corticoide/mockvision/backend/internal/store/db"
	"github.com/corticoide/mockvision/backend/internal/worker"
)

// prepareParams choose the renditions to prepare: those of one asset, or
// every one the cameras need.
type prepareParams struct {
	AssetID string `json:"asset_id,omitempty"`
}

// prepareCheckpoint is the plan and how far it went; a resumed job goes on
// with the renditions it had not reached.
type prepareCheckpoint struct {
	Renditions []string          `json:"renditions"`
	Done       map[string]string `json:"done"` // rendition → encoded, ready, skipped or gone
}

// PrepareResult summarizes a preparation.
type PrepareResult struct {
	Renditions int `json:"renditions"`
	Encoded    int `json:"encoded"`
	Ready      int `json:"ready"`
	Skipped    int `json:"skipped"`
}

func (cp prepareCheckpoint) result() PrepareResult {
	r := PrepareResult{Renditions: len(cp.Renditions)}
	for _, outcome := range cp.Done {
		switch outcome {
		case "encoded":
			r.Encoded++
		case "ready":
			r.Ready++
		case "skipped":
			r.Skipped++
		}
	}
	return r
}

func (s *Service) submitPrepare(ctx context.Context, actor Actor, p prepareParams) (worker.Job, bool, error) {
	title := "Prepare the cameras' renditions"
	if p.AssetID != "" {
		a, err := s.store.R().GetAsset(ctx, p.AssetID)
		if err != nil {
			if notFound(err) {
				return worker.Job{}, false, domain.Invalid("params.asset_id", "asset %s does not exist", p.AssetID)
			}
			return worker.Job{}, false, err
		}
		title = "Prepare the renditions of " + a.Filename
	}
	return s.jobs.Submit(ctx, worker.Spec{
		Type: JobPrepareRenditions, Title: title, Params: p, Key: JobPrepareRenditions + ":" + p.AssetID, CreatedBy: actorLabel(actor),
	})
}

// runPrepareRenditions encodes, one after the other, every rendition the
// cameras need, so starting many cameras later waits for nothing. When one
// fails it asks whether to retry, skip it or stop; unanswered, it skips.
func (s *Service) runPrepareRenditions(ctx context.Context, run *worker.Run) (any, error) {
	var p prepareParams
	if err := run.Params(&p); err != nil {
		return nil, err
	}
	var cp prepareCheckpoint
	if !run.Checkpoint(&cp) {
		run.Step("Planning", 0)
		ids, err := s.plannedRenditions(ctx, p.AssetID)
		if err != nil {
			return nil, err
		}
		cp = prepareCheckpoint{Renditions: ids, Done: map[string]string{}}
		if err := run.Save(cp); err != nil {
			return nil, err
		}
		run.Logf("the cameras need %d renditions", len(ids))
	}
	total := float64(max(len(cp.Renditions), 1))
	for i, id := range cp.Renditions {
		if _, done := cp.Done[id]; done {
			continue
		}
		from, to := float64(i)/total, float64(i+1)/total
		outcome, err := s.prepareOne(ctx, run, id, from, to)
		if err != nil {
			return cp.result(), err
		}
		cp.Done[id] = outcome
		if err := run.Save(cp); err != nil {
			return cp.result(), err
		}
		run.Progress(to)
	}
	res := cp.result()
	run.Logf("%d encoded, %d already ready, %d skipped", res.Encoded, res.Ready, res.Skipped)
	return res, nil
}

// prepareOne makes one rendition ready and says how: encoded, ready,
// skipped or gone.
func (s *Service) prepareOne(ctx context.Context, run *worker.Run, id string, from, to float64) (string, error) {
	r, asset, key, err := s.renditionInfo(ctx, id)
	if err != nil {
		return "gone", nil // deleted with its asset meanwhile
	}
	if s.lib.Ready(key) {
		if r.Status != string(domain.RenditionReady) {
			_ = s.store.W().SetRenditionStatus(ctx, db.SetRenditionStatusParams{ID: r.ID, Status: string(domain.RenditionReady), Sha256: key})
		}
		return "ready", nil
	}
	desc := fmt.Sprintf("%dx%d %s from %s", r.Width, r.Height, r.Codec, asset.Filename)
	for {
		err := s.encodeSteps(ctx, run, r, asset, key, from, to)
		if err == nil {
			_ = s.store.W().SetRenditionStatus(ctx, db.SetRenditionStatusParams{ID: r.ID, Status: string(domain.RenditionReady), Sha256: key})
			run.Logf("encoded %s", desc)
			return "encoded", nil
		}
		if ctx.Err() != nil {
			return "", err
		}
		_ = s.store.W().SetRenditionStatus(ctx, db.SetRenditionStatusParams{ID: r.ID, Status: string(domain.RenditionFailed), Sha256: key, Error: err.Error()})
		run.Logf("%s failed: %v", desc, err)
		answer, err := run.Ask(ctx, worker.Question{
			Text:    fmt.Sprintf("Encoding %s failed: %v", desc, err),
			Options: []worker.Option{{ID: "retry", Label: "Retry"}, {ID: "skip", Label: "Skip it"}, {ID: "stop", Label: "Stop the job"}},
			Default: "skip",
		}, questionTimeout)
		if err != nil {
			return "", err
		}
		switch answer {
		case "skip":
			return "skipped", nil
		case "stop":
			return "", worker.ErrStopped
		}
	}
}

// plannedRenditions lists, without repeats, the renditions the cameras'
// streams need now, optionally only those of one asset.
func (s *Service) plannedRenditions(ctx context.Context, assetID string) ([]string, error) {
	cams, err := s.store.R().ListCameras(ctx)
	if err != nil {
		return nil, err
	}
	var out []string
	seen := map[string]bool{}
	for _, c := range cams {
		b, err := s.loadBundle(ctx, c.ID)
		if err != nil {
			continue // deleted meanwhile
		}
		values := b.values()
		for _, st := range b.streams {
			if assetID != "" && st.AssetID != assetID {
				continue
			}
			_, rend, err := s.streamRendition(ctx, b, st, values)
			if err != nil {
				return nil, fmt.Errorf("camera %s, stream %s: %w", b.cam.Name, st.Stream, err)
			}
			if !seen[rend.ID] {
				seen[rend.ID] = true
				out = append(out, rend.ID)
			}
		}
	}
	return out, nil
}
