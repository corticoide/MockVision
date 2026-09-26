package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/corticoide/mockvision/backend/internal/domain"
	"github.com/corticoide/mockvision/backend/internal/pkg"
	"github.com/corticoide/mockvision/backend/internal/profile"
	"github.com/corticoide/mockvision/backend/internal/store"
	"github.com/corticoide/mockvision/backend/internal/store/db"
	"github.com/corticoide/mockvision/backend/internal/worker"
)

// inspectTimeout bounds the validation subprocess.
const inspectTimeout = 30 * time.Second

// ImportWait is how long an API call waits for its import job before it
// answers that the job goes on in the background.
const ImportWait = time.Minute

// importParams is what an import job needs, to run and to resume.
type importParams struct {
	Filename string `json:"filename"`
	Upload   string `json:"upload"` // file in the jobs directory
	Actor    Actor  `json:"actor"`
}

type importCheckpoint struct {
	Validated bool `json:"validated"`
}

// importOutcome is the result of an import job.
type importOutcome struct {
	Profile *ProfileView `json:"profile,omitempty"`
	Report  *pkg.Report  `json:"report,omitempty"`
	Created bool         `json:"created"`
	Error   *jobError    `json:"error,omitempty"`
}

// SubmitImport stores an uploaded package or loose profile.yaml and queues
// its import. The same content already being imported joins that job.
func (s *Service) SubmitImport(ctx context.Context, actor Actor, filename string, data []byte) (worker.Job, error) {
	if len(data) == 0 {
		return worker.Job{}, domain.Invalid("file", "the file is empty")
	}
	if len(data) > pkg.MaxPackageBytes {
		return worker.Job{}, domain.Invalid("file", "packages are limited to %d MB", pkg.MaxPackageBytes>>20)
	}
	sum := sha256.Sum256(data)
	// Each upload has its own file, so the cleanup of a finished import
	// never removes a new upload of the same content.
	upload := ulid.Make().String() + ".upload"
	path := filepath.Join(s.jobsDir(), upload)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return worker.Job{}, err
	}
	name := filepath.Base(filename)
	if name == "." || name == "/" {
		name = "package"
	}
	j, created, err := s.jobs.Submit(ctx, worker.Spec{
		Type: JobImport, Title: "Import " + name, Params: importParams{Filename: name, Upload: upload, Actor: actor},
		Key: "import:" + hex.EncodeToString(sum[:]), CreatedBy: actorLabel(actor),
	})
	if err != nil || !created {
		_ = os.Remove(path) // the job already importing this content has its own copy
	}
	return j, err
}

// ImportPackage imports a package and waits for the result: an immutable
// profile version (RN-02) or the report of why it was rejected.
func (s *Service) ImportPackage(ctx context.Context, actor Actor, filename string, data []byte) (*ImportResult, error) {
	j, err := s.SubmitImport(ctx, actor, filename, data)
	if err != nil {
		return nil, err
	}
	if j, err = s.jobs.Wait(ctx, j.ID); err != nil {
		return nil, err
	}
	return ImportOutcome(j)
}

// ImportOutcome turns a finished import job into its result, or into the
// error the import would have returned.
func ImportOutcome(j worker.Job) (*ImportResult, error) {
	var out importOutcome
	_ = json.Unmarshal(j.Result, &out)
	switch j.Status {
	case worker.Completed:
		if out.Profile == nil || out.Report == nil {
			return nil, errors.New("the import finished without a profile")
		}
		return &ImportResult{Profile: *out.Profile, Report: *out.Report, Created: out.Created}, nil
	case worker.Failed:
		if out.Error != nil && out.Error.Kind == "rejected" && out.Report != nil {
			return nil, &ImportError{Report: *out.Report}
		}
		if out.Error != nil {
			return nil, out.Error.err()
		}
		return nil, errors.New(j.Error)
	case worker.Interrupted:
		return nil, domain.Conflict("", "the import was interrupted by a restart; resume job %s", j.ID)
	}
	return nil, domain.Conflict("", "the import is %s: %s", j.Status, j.Error)
}

// runImport is the import job: validation in a subprocess, then the
// installation. The validated result is kept, so a resumed job does not
// validate twice.
func (s *Service) runImport(ctx context.Context, run *worker.Run) (any, error) {
	var p importParams
	if err := run.Params(&p); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(filepath.Join(s.jobsDir(), p.Upload))
	if err != nil {
		return nil, fmt.Errorf("the uploaded package is gone: %w", err)
	}
	resultPath := filepath.Join(s.jobsDir(), strings.TrimSuffix(p.Upload, ".upload")+".result.json")
	var res *pkg.Result
	var cp importCheckpoint
	if run.Checkpoint(&cp) && cp.Validated {
		if raw, err := os.ReadFile(resultPath); err == nil {
			var r pkg.Result
			if json.Unmarshal(raw, &r) == nil {
				res = &r
				run.Logf("validation kept from before the restart")
			}
		}
	}
	if res == nil {
		run.Step("Validating", 0.05)
		sctx, cancel := run.StepContext(ctx)
		res, err = s.inspect(sctx, p.Filename, data)
		cancel()
		if err != nil {
			return importOutcome{Error: describeError(err)}, err
		}
		raw, _ := json.Marshal(res)
		if err := os.WriteFile(resultPath, raw, 0o600); err != nil {
			return nil, err
		}
		if err := run.Save(importCheckpoint{Validated: true}); err != nil {
			return nil, err
		}
	}
	rep := res.Report
	run.Logf("%s %s@%s: level %s, signature %s", rep.Kind, rep.ID, rep.Version, rep.Level, rep.Signature)
	run.Step("Installing", 0.8)
	out, err := s.installPackage(ctx, p.Actor, data, res)
	if err != nil {
		return importOutcome{Report: &rep, Error: describeError(err)}, err
	}
	return importOutcome{Profile: &out.Profile, Report: &out.Report, Created: out.Created}, nil
}

// installPackage installs a validated package as an immutable version
// (RN-02); installing the same content again changes nothing.
func (s *Service) installPackage(ctx context.Context, actor Actor, data []byte, res *pkg.Result) (*ImportResult, error) {
	rep := res.Report
	if !rep.OK() {
		return nil, &ImportError{Report: rep}
	}
	if rep.Kind != "profile" || res.Profile == nil {
		return nil, domain.Invalid("file", "only profile packages can be installed in this version")
	}

	existing, err := s.store.R().GetPackageByKey(ctx, db.GetPackageByKeyParams{Kind: rep.Kind, PkgID: rep.ID, Version: rep.Version})
	if err == nil {
		if existing.Sha256 != rep.SHA256 {
			return nil, domain.Conflict("version", "%s@%s is already installed with different content; publish a new version", rep.ID, rep.Version)
		}
		prof, err := s.store.R().GetProfileByRef(ctx, db.GetProfileByRefParams{ProfileID: rep.ID, Version: rep.Version})
		if err != nil {
			return nil, err
		}
		return &ImportResult{Profile: profileView(prof, existing.SignatureStatus, 0), Report: rep}, nil
	} else if !notFound(err) {
		return nil, err
	}

	ext := ".mvpkg"
	if !bytes.HasPrefix(data, []byte("PK")) {
		ext = ".yaml"
	}
	path := filepath.Join(s.opts.DataDir, "packages", rep.SHA256+ext)
	if err := os.WriteFile(path, data, 0o640); err != nil {
		return nil, err
	}
	manifest, _ := json.Marshal(res.Manifest)
	report, _ := json.Marshal(rep)
	firmware, _ := json.Marshal(nonNil(res.Profile.Firmware))
	coverage, _ := json.Marshal(rep.Coverage)
	if rep.Coverage == nil {
		coverage = []byte("{}")
	}
	now := time.Now().UnixMilli()
	pkgID := ulid.Make().String()
	profID := ulid.Make().String()
	err = s.store.Tx(ctx, func(q *db.Queries) error {
		if err := q.InsertPackage(ctx, db.InsertPackageParams{
			ID: pkgID, Kind: rep.Kind, PkgID: rep.ID, Version: rep.Version, Sha256: rep.SHA256, SignatureStatus: rep.Signature,
			ManifestJson: string(manifest), ReportJson: string(report), Enabled: 1, InstalledAt: now,
		}); err != nil {
			return err
		}
		return q.InsertProfile(ctx, db.InsertProfileParams{
			ID: profID, PackageID: pkgID, ProfileID: rep.ID, Version: rep.Version, Name: res.Profile.Name, Vendor: res.Profile.Vendor,
			Model: res.Profile.Model, FirmwareJson: string(firmware), ResolvedJson: string(res.Resolved), Level: rep.Level,
			CoverageJson: string(coverage), CreatedAt: now,
		})
	})
	if err != nil {
		if store.IsUnique(err) {
			return nil, domain.Conflict("version", "%s@%s is already installed", rep.ID, rep.Version)
		}
		return nil, err
	}
	s.audit(ctx, actor, "package.import", "profile", profID, map[string]any{"id": rep.ID, "version": rep.Version, "level": rep.Level, "sha256": rep.SHA256})
	prof, err := s.store.R().GetProfile(ctx, profID)
	if err != nil {
		return nil, err
	}
	view := profileView(prof, rep.Signature, 0)
	s.pub.Publish("profiles", "installed", view)
	return &ImportResult{Profile: view, Report: rep, Created: true}, nil
}

// inspect runs the import pipeline in an unprivileged subprocess, so a
// hostile package can at worst crash that process.
func (s *Service) inspect(ctx context.Context, filename string, data []byte) (*pkg.Result, error) {
	if s.opts.Exe == "" {
		return pkg.Inspect(data, filename, s.catalog), nil
	}
	ctx, cancel := context.WithTimeout(ctx, inspectTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, s.opts.Exe, "pkg", "inspect", "--name", filepath.Base(filename))
	cmd.Stdin = bytes.NewReader(data)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.Env = []string{"PATH=/usr/bin:/bin"}
	cmd.WaitDelay = 2 * time.Second
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, errors.New("package validation timed out")
		}
		return nil, fmt.Errorf("package validation failed: %v: %s", err, bytes.TrimSpace(stderr.Bytes()))
	}
	var res pkg.Result
	if err := json.Unmarshal(stdout.Bytes(), &res); err != nil {
		return nil, fmt.Errorf("package validation returned invalid output: %w", err)
	}
	return &res, nil
}

func profileView(p db.Profile, signature string, cameras int64) ProfileView {
	var fw []string
	_ = json.Unmarshal([]byte(p.FirmwareJson), &fw)
	var cov map[string]string
	_ = json.Unmarshal([]byte(p.CoverageJson), &cov)
	return ProfileView{ID: p.ID, ProfileID: p.ProfileID, Version: p.Version, Name: p.Name, Vendor: p.Vendor, Model: p.Model,
		Firmware: nonNil(fw), Level: p.Level, SignatureStatus: signature, Archived: store.Bool(p.Archived), CameraCount: int(cameras),
		CreatedAt: store.Time(p.CreatedAt), Coverage: cov}
}

// ListProfiles returns installed profile versions.
func (s *Service) ListProfiles(ctx context.Context) ([]ProfileView, error) {
	rows, err := s.store.R().ListProfiles(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]ProfileView, 0, len(rows))
	for _, r := range rows {
		out = append(out, profileView(db.Profile{ID: r.ID, PackageID: r.PackageID, ProfileID: r.ProfileID, Version: r.Version, Name: r.Name,
			Vendor: r.Vendor, Model: r.Model, FirmwareJson: r.FirmwareJson, Level: r.Level, CoverageJson: r.CoverageJson,
			Archived: r.Archived, CreatedAt: r.CreatedAt}, r.SignatureStatus, r.CameraCount))
	}
	return out, nil
}

// GetProfile returns a profile version with what the camera wizard needs.
func (s *Service) GetProfile(ctx context.Context, profileID, version string) (*ProfileDetail, error) {
	p, err := s.store.R().GetProfileByRef(ctx, db.GetProfileByRefParams{ProfileID: profileID, Version: version})
	if err != nil {
		return nil, store.NotFound(err)
	}
	pk, err := s.store.R().GetPackage(ctx, p.PackageID)
	if err != nil {
		return nil, err
	}
	doc, err := s.profileDoc(p)
	if err != nil {
		return nil, err
	}
	d := &ProfileDetail{ProfileView: profileView(p, pk.SignatureStatus, 0), Streams: []ProfileStreamView{}, Engines: []ProfileEngineView{},
		Events: []string{}, FactoryUsers: []UserView{}, Params: []ParamView{}}
	for _, name := range profile.SortedKeys(doc.Media.Streams) {
		st := doc.Media.Streams[name]
		sv := ProfileStreamView{Name: name, Codecs: st.Codecs, Resolutions: st.Resolutions}
		if st.FPS != nil {
			sv.FPSMin, sv.FPSMax = st.FPS.Min, st.FPS.Max
		}
		sv.Default.Codec, sv.Default.Resolution, sv.Default.FPS = st.Default.Codec, st.Default.Resolution, st.Default.FPS
		sv.Default.Bitrate, sv.Default.GOP = st.Default.Bitrate, st.Default.GOP
		d.Streams = append(d.Streams, sv)
	}
	for _, inst := range profile.SortedKeys(doc.Engines) {
		name, _, _ := profile.EngineName(doc.Engines[inst])
		port, _ := s.instancePort(doc, inst)
		d.Engines = append(d.Engines, ProfileEngineView{Instance: inst, Engine: name, Port: port})
	}
	d.Events = append(d.Events, profile.SortedKeys(doc.Events)...)
	for _, u := range doc.Identity.Factory.Users {
		d.FactoryUsers = append(d.FactoryUsers, UserView{Username: u.Username, Role: u.Role})
	}
	d.FactoryIP = doc.Identity.Factory.Network.IP
	for _, key := range profile.SortedKeys(doc.State) {
		p := doc.State[key]
		d.Params = append(d.Params, ParamView{Key: key, Type: p.Type, Value: p.Default, Default: p.Default, Values: p.Values,
			Min: p.Min, Max: p.Max, Bind: p.Bind, Effective: p.Bind != "", Description: p.Description, Origin: "profile"})
	}
	return d, nil
}

// ArchiveProfile stops offering a profile for new cameras (RN-03: profiles
// in use are archived, never deleted).
func (s *Service) ArchiveProfile(ctx context.Context, actor Actor, profileID, version string, archived bool) (*ProfileDetail, error) {
	p, err := s.store.R().GetProfileByRef(ctx, db.GetProfileByRefParams{ProfileID: profileID, Version: version})
	if err != nil {
		return nil, store.NotFound(err)
	}
	if err := s.store.W().SetProfileArchived(ctx, db.SetProfileArchivedParams{ID: p.ID, Archived: store.Int(archived)}); err != nil {
		return nil, err
	}
	s.audit(ctx, actor, "profile.archive", "profile", p.ID, map[string]any{"archived": archived})
	return s.GetProfile(ctx, profileID, version)
}
