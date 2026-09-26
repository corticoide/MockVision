package app

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/corticoide/mockvision/backend/internal/domain"
	"github.com/corticoide/mockvision/backend/internal/store"
	"github.com/corticoide/mockvision/backend/internal/store/db"
	"github.com/corticoide/mockvision/backend/internal/worker"
)

func waitJob(t *testing.T, svc *Service, id string) worker.Job {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	j, err := svc.WaitJob(ctx, id)
	if err != nil {
		t.Fatalf("job %s: %v (status %s)", id, err, j.Status)
	}
	return j
}

func jobFiles(t *testing.T, svc *Service) []string {
	t.Helper()
	entries, err := os.ReadDir(svc.jobsDir())
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

func demoProfile(t *testing.T, version string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "profiles", "milesight-demo.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return []byte(strings.Replace(string(data), "version: 0.1.0", "version: "+version, 1))
}

func TestImportRunsAsAJob(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	data := demoProfile(t, "0.2.0")

	a, err := svc.SubmitImport(ctx, testActor, "demo.yaml", data)
	if err != nil {
		t.Fatal(err)
	}
	b, err := svc.SubmitImport(ctx, testActor, "again.yaml", data)
	if err != nil || b.ID != a.ID {
		t.Fatalf("the same content joins the open import: %s vs %s, %v", b.ID, a.ID, err)
	}
	j := waitJob(t, svc, a.ID)
	res, err := ImportOutcome(j)
	if err != nil || !res.Created || res.Profile.Version != "0.2.0" || j.Title != "Import demo.yaml" || j.CreatedBy != "test" {
		t.Fatalf("import: %+v %+v %v", j, res, err)
	}
	detail, err := svc.GetJob(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	var steps []string
	for _, e := range detail.Events {
		if e.Kind == worker.EventStep {
			var d struct{ Step string }
			_ = json.Unmarshal(e.Data, &d)
			steps = append(steps, d.Step)
		}
	}
	if strings.Join(steps, ",") != "Validating,Installing" {
		t.Fatalf("steps %v", steps)
	}
	if files := jobFiles(t, svc); len(files) != 0 {
		t.Fatalf("an import leaves no files behind: %v", files)
	}

	// A rejected package fails with its report, as the synchronous import did.
	_, err = svc.ImportPackage(ctx, testActor, "bad.yaml", []byte("schema: 1\nprofile: {id: nope}\n"))
	var ierr *ImportError
	if !errors.As(err, &ierr) || len(ierr.Report.Problems) == 0 {
		t.Fatalf("bad package: %v", err)
	}
	// Different content for an installed version is a conflict.
	changed := []byte(strings.Replace(string(data), "Network Camera", "Other Camera", 1))
	_, err = svc.ImportPackage(ctx, testActor, "changed.yaml", changed)
	var conflict *domain.ConflictError
	if !errors.As(err, &conflict) || conflict.Field != "version" {
		t.Fatalf("changed content: %v", err)
	}
	if _, err := svc.SubmitImport(ctx, testActor, "empty.yaml", nil); err == nil {
		t.Fatal("an empty upload was queued")
	}
}

// An import stopped after its validation resumes with the installation.
func TestImportResumesAfterValidation(t *testing.T) {
	svc, _ := newBareService(t)
	ctx := context.Background()
	data := demoProfile(t, "0.3.0")
	j, err := svc.SubmitImport(ctx, testActor, "demo.yaml", data)
	if err != nil {
		t.Fatal(err)
	}
	// The node stopped right after validating: result on disk, checkpoint
	// saved, job interrupted.
	var p importParams
	if err := j.DecodeParams(&p); err != nil {
		t.Fatal(err)
	}
	res, err := svc.inspect(ctx, p.Filename, data)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(res)
	if err := os.WriteFile(filepath.Join(svc.jobsDir(), strings.TrimSuffix(p.Upload, ".upload")+".result.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	err = svc.store.W().UpdateJob(ctx, db.UpdateJobParams{
		ID: j.ID, Status: string(worker.Interrupted), Progress: 0.5, Step: "Validating", CheckpointJson: `{"validated":true}`,
		ResultJson: "{}", StartedAt: nullMillis(now),
	})
	if err != nil {
		t.Fatal(err)
	}

	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		_ = svc.Run(runCtx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	if got, _ := svc.jobs.Get(ctx, j.ID); got.Status != worker.Interrupted {
		t.Fatalf("before resuming: %s", got.Status)
	}
	if _, err := svc.ResumeJob(ctx, testActor, j.ID); err != nil {
		t.Fatal(err)
	}
	final := waitJob(t, svc, j.ID)
	if final.Status != worker.Completed {
		t.Fatalf("resumed import: %+v", final)
	}
	detail, _ := svc.GetJob(ctx, j.ID)
	kept := false
	for _, e := range detail.Events {
		if e.Kind == worker.EventStep && strings.Contains(string(e.Data), "Validating") {
			t.Fatal("the resumed import validated again")
		}
		if e.Kind == worker.EventLog && strings.Contains(string(e.Data), "validation kept") {
			kept = true
		}
	}
	if !kept {
		t.Fatal("the resumed import does not say it kept the validation")
	}
	audit, _ := svc.ListAudit(ctx, AuditFilter{Action: "job.resume"})
	if len(audit.Items) != 1 {
		t.Fatalf("resume audited %d times", len(audit.Items))
	}
}

func TestPrepareRenditions(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	a := createCamera(t, svc, "Prep A", false)
	createCamera(t, svc, "Prep B", false) // same settings: same rendition
	c := createCamera(t, svc, "Prep C", false)
	if _, err := svc.UpdateCameraStream(ctx, testActor, c.ID, "main", StreamUpdate{Resolution: ptr("640x360")}); err != nil {
		t.Fatal(err)
	}

	j, err := svc.CreateJob(ctx, testActor, JobInput{Type: JobPrepareRenditions})
	if err != nil {
		t.Fatal(err)
	}
	done := waitJob(t, svc, j.ID)
	var res PrepareResult
	_ = json.Unmarshal(done.Result, &res)
	if done.Status != worker.Completed || res.Renditions != 2 || res.Encoded+res.Ready != 2 || res.Skipped != 0 {
		t.Fatalf("prepare: %+v %+v", done, res)
	}

	// An image whose file vanished cannot be encoded: the job asks.
	up, err := svc.UploadAsset(ctx, testActor, "photo.jpg", strings.NewReader(string(testJPEG(t))))
	if err != nil {
		t.Fatal(err)
	}
	asset, _ := svc.store.R().GetAsset(ctx, up.ID)
	if err := os.Remove(svc.lib.AssetPath(asset.Sha256, assetExt(asset))); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.UpdateCameraStream(ctx, testActor, a.ID, "main", StreamUpdate{AssetID: &up.ID}); err != nil {
		t.Fatal(err)
	}
	j, err = svc.CreateJob(ctx, testActor, JobInput{Type: JobPrepareRenditions, Params: json.RawMessage(`{"asset_id":"` + up.ID + `"}`)})
	if err != nil {
		t.Fatal(err)
	}
	waiting := func() worker.Job {
		deadline := time.Now().Add(30 * time.Second)
		for {
			got, _ := svc.jobs.Get(ctx, j.ID)
			if got.Status == worker.Waiting && got.Question != nil {
				return got
			}
			if time.Now().After(deadline) || got.Status.Final() {
				t.Fatalf("no question: %+v", got)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	q := waiting()
	if !strings.Contains(q.Question.Text, "photo.jpg") || q.Question.Default != "skip" {
		t.Fatalf("question: %+v", q.Question)
	}
	if _, err := svc.AnswerJob(ctx, testActor, j.ID, "retry"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	waiting() // it fails again and asks again
	if _, err := svc.AnswerJob(ctx, testActor, j.ID, "skip"); err != nil {
		t.Fatal(err)
	}
	done = waitJob(t, svc, j.ID)
	res = PrepareResult{}
	_ = json.Unmarshal(done.Result, &res)
	if done.Status != worker.Completed || res.Skipped != 1 || !strings.HasSuffix(done.Title, "photo.jpg") {
		t.Fatalf("after skipping: %+v %+v", done, res)
	}
	audit, _ := svc.ListAudit(ctx, AuditFilter{Action: "job"})
	counts := map[string]int{}
	for _, e := range audit.Items {
		counts[e.Action]++
	}
	if counts["job.create"] != 2 || counts["job.answer"] != 2 {
		t.Fatalf("job audit: %v", counts)
	}

	if _, err := svc.CreateJob(ctx, testActor, JobInput{Type: JobImport}); err == nil {
		t.Fatal("an import job without its upload was created")
	}
	if _, err := svc.CreateJob(ctx, testActor, JobInput{Type: JobPrepareRenditions, Params: json.RawMessage(`{"asset_id":"missing"}`)}); err == nil {
		t.Fatal("a preparation of a missing asset was created")
	}
}

// Stopping a camera while its stream encodes answers at once.
func TestStopWhileTheStreamEncodes(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no shell")
	}
	slow := filepath.Join(t.TempDir(), "slow-ffmpeg")
	script := "#!/bin/sh\ncase \"$*\" in *libx264*) sleep 30;; esac\nexec ffmpeg \"$@\"\n"
	if err := os.WriteFile(slow, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	svc := newTestServiceWith(t, slow)
	ctx := context.Background()
	cam := createCamera(t, svc, "Slow", false)
	if _, err := svc.StartCamera(ctx, testActor, cam.ID); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond) // provisioning, waiting for the encode
	if v, _ := svc.GetCamera(ctx, cam.ID); v.Status.State != string(domain.StateProvisioning) {
		t.Fatalf("state %s, want provisioning", v.Status.State)
	}
	start := time.Now()
	sctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	v, err := svc.StopCamera(sctx, testActor, cam.ID)
	if err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 3*time.Second || v.Status.State != string(domain.StateStopped) {
		t.Fatalf("stop took %s, state %s", took, v.Status.State)
	}
	// The encoding job goes on; canceling it ends FFmpeg.
	jobs, _ := svc.ListJobs(ctx, JobFilter{Type: JobRendition, Status: "active"})
	if len(jobs.Items) == 0 {
		t.Fatal("the rendition job is gone")
	}
	for _, j := range jobs.Items {
		if _, err := svc.CancelJob(ctx, testActor, j.ID); err != nil {
			t.Fatal(err)
		}
		if got := waitJob(t, svc, j.ID); got.Status != worker.Canceled {
			t.Fatalf("canceled rendition: %s", got.Status)
		}
	}
}

func nullMillis(ms int64) sql.NullInt64 { return store.NullMillis(time.UnixMilli(ms)) }

// testJPEG is a small photo-like image.
func testJPEG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 64, 48))
	for y := 0; y < 48; y++ {
		for x := 0; x < 64; x++ {
			img.Set(x, y, color.RGBA{uint8(x * 4), uint8(y * 5), 90, 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
