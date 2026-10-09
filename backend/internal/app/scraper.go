package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/corticoide/mockvision/backend/internal/domain"
	"github.com/corticoide/mockvision/backend/internal/pkg"
	"github.com/corticoide/mockvision/backend/internal/scraper"
	"github.com/corticoide/mockvision/backend/internal/store"
	"github.com/corticoide/mockvision/backend/internal/store/db"
	"github.com/corticoide/mockvision/backend/internal/worker"
)

// JobCapture runs a capture program against a device.
const JobCapture = "scraper.capture"

// ProgramView is a capture program: a read-only recipe, built into the
// binary or installed as a package.
type ProgramView struct {
	ProgramID string   `json:"program_id"`
	Version   string   `json:"version"`
	Name      string   `json:"name"`
	Vendors   []string `json:"vendors"`
	Steps     int      `json:"steps"`
	Source    string   `json:"source"` // builtin | installed
	Signature string   `json:"signature,omitempty"`
}

func programView(p *scraper.Program, source, signature string) ProgramView {
	return ProgramView{ProgramID: p.ID, Version: p.Version, Name: p.Name, Vendors: nonNil(p.Compatible.Vendors),
		Steps: len(p.Steps), Source: source, Signature: signature}
}

// ListPrograms returns the capture programs: the built-in ones and any
// installed as packages. A device narrows them to what suits its vendor.
func (s *Service) ListPrograms(ctx context.Context, vendor string) ([]ProgramView, error) {
	out := []ProgramView{}
	for _, p := range scraper.Builtin() {
		if vendor == "" || p.Matches(vendor) {
			out = append(out, programView(p, "builtin", ""))
		}
	}
	rows, err := s.store.R().ListPrograms(ctx)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		var steps []scraper.Step
		_ = json.Unmarshal([]byte(r.StepsJson), &steps)
		var comp scraper.Compatible
		_ = json.Unmarshal([]byte(r.CompatibleJson), &comp)
		p := &scraper.Program{ID: r.ProgramID, Version: r.Version, Name: r.Name, Compatible: comp, Steps: steps}
		if vendor == "" || p.Matches(vendor) {
			out = append(out, programView(p, "installed", r.SignatureStatus))
		}
	}
	return out, nil
}

// programFor resolves a program by "id" or "id@version": a built-in or an
// installed one. Empty version takes the newest match.
func (s *Service) programFor(ctx context.Context, ref string) (*scraper.Program, error) {
	id, version, _ := cut(ref, "@")
	for _, p := range scraper.Builtin() {
		if p.ID == id && (version == "" || p.Version == version) {
			return p, nil
		}
	}
	rows, err := s.store.R().ListPrograms(ctx)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		if r.ProgramID == id && (version == "" || r.Version == version) {
			var steps []scraper.Step
			var comp scraper.Compatible
			if err := errors.Join(json.Unmarshal([]byte(r.StepsJson), &steps), json.Unmarshal([]byte(r.CompatibleJson), &comp)); err != nil {
				return nil, err
			}
			return &scraper.Program{ID: r.ProgramID, Version: r.Version, Name: r.Name, Compatible: comp, Steps: steps}, nil
		}
	}
	return nil, domain.Invalid("program", "unknown capture program %q", ref)
}

// installProgram installs a validated program package.
func (s *Service) installProgram(ctx context.Context, actor Actor, res *pkg.Result, source string) (*ImportResult, error) {
	rep, pi := res.Report, res.Program
	if pi == nil {
		return nil, errors.New("the program package was validated without its program")
	}
	existing, err := s.store.R().GetPackageByKey(ctx, db.GetPackageByKeyParams{Kind: rep.Kind, PkgID: rep.ID, Version: rep.Version})
	if err == nil {
		if existing.Sha256 != rep.SHA256 {
			return nil, domain.Conflict("version", "%s@%s is already installed with different content", rep.ID, rep.Version)
		}
		v, gerr := s.programByPackage(ctx, existing.ID)
		if gerr != nil {
			return nil, gerr
		}
		return &ImportResult{Program: v, Report: rep}, nil
	} else if !notFound(err) {
		return nil, err
	}
	prog, err := scraper.ParseProgram(pi.ProgramRaw)
	if err != nil {
		return nil, err
	}
	steps, _ := json.Marshal(prog.Steps)
	comp, _ := json.Marshal(prog.Compatible)
	manifest, _ := json.Marshal(res.Manifest)
	report, _ := json.Marshal(rep)
	now := time.Now().UnixMilli()
	pkgID, progID := ulid.Make().String(), ulid.Make().String()
	err = s.store.Tx(ctx, func(q *db.Queries) error {
		if err := q.InsertPackage(ctx, db.InsertPackageParams{
			ID: pkgID, Kind: rep.Kind, PkgID: rep.ID, Version: rep.Version, Sha256: rep.SHA256, SignatureStatus: rep.Signature,
			Signer: rep.Signer, ManifestJson: string(manifest), ReportJson: string(report), Enabled: 1, InstalledAt: now, Source: source,
		}); err != nil {
			return err
		}
		return q.InsertProgram(ctx, db.InsertProgramParams{ID: progID, PackageID: pkgID, ProgramID: prog.ID, Version: prog.Version,
			Name: prog.Name, CompatibleJson: string(comp), StepsJson: string(steps), InstalledAt: now})
	})
	if err != nil {
		if store.IsUnique(err) {
			return nil, domain.Conflict("version", "program %s %s is already installed", prog.ID, prog.Version)
		}
		return nil, err
	}
	s.audit(ctx, actor, "package.import", "program", progID, map[string]any{"id": prog.ID, "version": prog.Version, "steps": len(prog.Steps)})
	v := programView(prog, "installed", rep.Signature)
	return &ImportResult{Program: &v, Report: rep, Created: true}, nil
}

func (s *Service) programByPackage(ctx context.Context, packageID string) (*ProgramView, error) {
	rows, err := s.store.R().ListPrograms(ctx)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		if r.PackageID == packageID {
			var comp scraper.Compatible
			_ = json.Unmarshal([]byte(r.CompatibleJson), &comp)
			var steps []scraper.Step
			_ = json.Unmarshal([]byte(r.StepsJson), &steps)
			v := programView(&scraper.Program{ID: r.ProgramID, Version: r.Version, Name: r.Name, Compatible: comp, Steps: steps}, "installed", r.SignatureStatus)
			return &v, nil
		}
	}
	return nil, domain.ErrNotFound
}

// --- Captures ---

// captureParams is what a capture job runs with.
type captureParams struct {
	DeviceID   string `json:"device_id"`
	ProgramRef string `json:"program_ref"`
	CaptureID  string `json:"capture_id"`
}

// CaptureView is a capture: a program run against a device, read-only.
type CaptureView struct {
	ID             string                 `json:"id"`
	DeviceID       string                 `json:"device_id"`
	JobID          string                 `json:"job_id,omitempty"`
	Program        string                 `json:"program"`
	Status         string                 `json:"status"`
	Result         *scraper.CaptureResult `json:"result,omitempty"`
	DraftProfileID string                 `json:"draft_profile_id,omitempty"`
	CreatedAt      time.Time              `json:"created_at"`
	FinishedAt     *time.Time             `json:"finished_at,omitempty"`
}

func captureView(c db.Capture) CaptureView {
	v := CaptureView{ID: c.ID, DeviceID: c.DeviceID, JobID: c.JobID, Program: c.ProgramRef, Status: c.Status, CreatedAt: store.Time(c.CreatedAt)}
	if c.ArtifactsJson != "" && c.ArtifactsJson != "{}" {
		var r scraper.CaptureResult
		if json.Unmarshal([]byte(c.ArtifactsJson), &r) == nil {
			v.Result = &r
		}
	}
	if c.DraftProfileID.Valid {
		v.DraftProfileID = c.DraftProfileID.String
	}
	if c.FinishedAt.Valid {
		t := store.Time(c.FinishedAt.Int64)
		v.FinishedAt = &t
	}
	return v
}

// StartCapture runs a capture program against a device, read-only, as a
// background job. It refuses until the device is authorized (RN-17).
func (s *Service) StartCapture(ctx context.Context, actor Actor, deviceID, programRef string) (*CaptureView, error) {
	d, err := s.store.R().GetDevice(ctx, deviceID)
	if err != nil {
		return nil, store.NotFound(err)
	}
	if !store.Bool(d.Authorized) {
		return nil, domain.Conflict("authorized", "confirm you may capture this device before running a program (RN-17)")
	}
	prog, err := s.programFor(ctx, programRef)
	if err != nil {
		return nil, err
	}
	id := ulid.Make().String()
	now := time.Now().UnixMilli()
	if err := s.store.W().InsertCapture(ctx, db.InsertCaptureParams{ID: id, DeviceID: deviceID, ProgramRef: prog.ID + "@" + prog.Version,
		Status: "running", ArtifactsJson: "{}", CreatedAt: now}); err != nil {
		return nil, err
	}
	j, _, err := s.jobs.Submit(ctx, worker.Spec{Type: JobCapture, Title: "Capture " + d.Name + " with " + prog.Name,
		Params: captureParams{DeviceID: deviceID, ProgramRef: prog.ID + "@" + prog.Version, CaptureID: id}, CreatedBy: actorLabel(actor)})
	if err != nil {
		return nil, err
	}
	_ = s.store.W().SetCaptureJob(ctx, db.SetCaptureJobParams{ID: id, JobID: j.ID})
	s.audit(ctx, actor, "scraper.capture", "device", deviceID, map[string]any{"program": prog.ID, "capture": id})
	return s.GetCapture(ctx, id)
}

// runCapture is the capture job: a read-only run of a program against a
// device (RN-17), its recordings stored on the capture.
func (s *Service) runCapture(ctx context.Context, run *worker.Run) (any, error) {
	var p captureParams
	if err := run.Params(&p); err != nil {
		return nil, err
	}
	d, err := s.store.R().GetDevice(ctx, p.DeviceID)
	if err != nil {
		return nil, err
	}
	prog, err := s.programFor(ctx, p.ProgramRef)
	if err != nil {
		return nil, err
	}
	t, err := s.target(d)
	if err != nil {
		return nil, err
	}
	run.Step("Capturing", 0.1)
	prober := scraper.NewProber(t, float64(s.scrapeRate()))
	res := scraper.RunProgram(ctx, prober, prog)
	var det scraper.Detected
	_ = json.Unmarshal([]byte(d.DetectedJson), &det)
	res.Vendor = det.Vendor
	raw, _ := json.Marshal(res)
	status := "done"
	if res.OK == 0 {
		status = "failed"
	}
	now := time.Now().UnixMilli()
	if err := s.store.W().FinishCapture(ctx, db.FinishCaptureParams{ID: p.CaptureID, Status: status, ArtifactsJson: string(raw),
		FinishedAt: sql.NullInt64{Int64: now, Valid: true}}); err != nil {
		return nil, err
	}
	run.Logf("captured %d of %d steps from %s", res.OK, res.Steps, d.Name)
	s.pub.Publish("scraper", "capture", map[string]any{"id": p.CaptureID, "status": status})
	return map[string]any{"capture_id": p.CaptureID, "ok": res.OK, "steps": res.Steps}, nil
}

// GetCapture returns a capture with its recordings.
func (s *Service) GetCapture(ctx context.Context, id string) (*CaptureView, error) {
	c, err := s.store.R().GetCapture(ctx, id)
	if err != nil {
		return nil, store.NotFound(err)
	}
	v := captureView(c)
	return &v, nil
}

// ListCaptures returns the captures of a device, newest first.
func (s *Service) ListCaptures(ctx context.Context, deviceID string) ([]CaptureView, error) {
	rows, err := s.store.R().ListCapturesByDevice(ctx, db.ListCapturesByDeviceParams{DeviceID: deviceID, Lim: 100})
	if err != nil {
		return nil, err
	}
	out := make([]CaptureView, 0, len(rows))
	for _, c := range rows {
		out = append(out, captureView(c))
	}
	return out, nil
}

func cut(s, sep string) (string, string, bool) {
	return strings.Cut(s, sep)
}
