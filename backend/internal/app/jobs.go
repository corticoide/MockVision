package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/corticoide/mockvision/backend/internal/domain"
	"github.com/corticoide/mockvision/backend/internal/worker"
)

// Job types.
const (
	JobRendition         = "rendition"
	JobImport            = "import"
	JobPrepareRenditions = "renditions.prepare"
)

// JobRetention is how long finished jobs and their history are kept.
const JobRetention = 30 * 24 * time.Hour

// questionTimeout is how long a job waits for the user before it takes
// the default answer.
const questionTimeout = 10 * time.Minute

// JobDetail is a job with its history.
type JobDetail struct {
	worker.Job
	Events []worker.Event `json:"events"`
}

// JobInput creates a job from the API. Only jobs that make sense on their
// own can be created this way; imports come with their upload and
// renditions with the cameras that need them.
type JobInput struct {
	Type   string          `json:"type"`
	Params json.RawMessage `json:"params"`
}

// JobFilter selects jobs.
type JobFilter = worker.Filter

// publisher adapts the service's publisher, which tests may swap, to the
// runner.
type publisher struct{ s *Service }

func (p publisher) Publish(topic, typ string, data any) { p.s.pub.Publish(topic, typ, data) }
func (p publisher) Forget(topic string)                 { p.s.pub.Forget(topic) }

func (s *Service) newRunner() *worker.Runner {
	r := worker.New(worker.Config{
		Store: s.store, Publisher: publisher{s}, Log: s.log.With("component", "jobs"),
		MaxRunning:  func() int { return s.Settings(s.baseCtx).MaxJobs },
		StepTimeout: func() time.Duration { return time.Duration(s.Settings(s.baseCtx).JobStepTimeoutSeconds) * time.Second },
	})
	r.Register(JobRendition, worker.Kind{Handler: s.runRendition})
	r.Register(JobImport, worker.Kind{Handler: s.runImport, Cleanup: s.removeImportFiles})
	r.Register(JobPrepareRenditions, worker.Kind{Handler: s.runPrepareRenditions})
	return r
}

// jobsDir keeps what jobs need to resume, such as an uploaded package.
func (s *Service) jobsDir() string { return filepath.Join(s.opts.DataDir, "jobs") }

// removeImportFiles deletes an import's upload and validation once the job
// is over.
func (s *Service) removeImportFiles(j worker.Job) {
	var p importParams
	if j.DecodeParams(&p) != nil || p.Upload == "" || filepath.Base(p.Upload) != p.Upload {
		return
	}
	base := strings.TrimSuffix(p.Upload, ".upload")
	for _, name := range []string{p.Upload, base + ".result.json"} {
		_ = os.Remove(filepath.Join(s.jobsDir(), name))
	}
}

// actorLabel names who submitted a job.
func actorLabel(a Actor) string {
	switch {
	case a.Token != nil:
		return actorName(a) + " (token " + a.Token.Name + ")"
	case a.Type == "system":
		return "node"
	}
	return actorName(a)
}

// ListJobs returns jobs, newest first.
func (s *Service) ListJobs(ctx context.Context, f JobFilter) (Page[worker.Job], error) {
	items, next, err := s.jobs.List(ctx, f)
	if err != nil {
		return Page[worker.Job]{}, err
	}
	return Page[worker.Job]{Items: items, NextCursor: next}, nil
}

// GetJob returns a job with its history.
func (s *Service) GetJob(ctx context.Context, id string) (*JobDetail, error) {
	j, err := s.jobs.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	events, err := s.jobs.Events(ctx, id, 0, 0)
	if err != nil {
		return nil, err
	}
	return &JobDetail{Job: j, Events: events}, nil
}

// CreateJob starts a job requested through the API.
func (s *Service) CreateJob(ctx context.Context, actor Actor, in JobInput) (worker.Job, error) {
	switch in.Type {
	case JobPrepareRenditions:
		var p prepareParams
		if len(in.Params) > 0 && string(in.Params) != "null" {
			if err := json.Unmarshal(in.Params, &p); err != nil {
				return worker.Job{}, domain.Invalid("params", "%v", err)
			}
		}
		j, created, err := s.submitPrepare(ctx, actor, p)
		if err != nil {
			return j, err
		}
		if created {
			s.audit(ctx, actor, "job.create", "job", j.ID, map[string]any{"name": j.Title, "type": j.Type})
		}
		return j, nil
	case JobImport:
		return worker.Job{}, domain.Invalid("type", "packages are imported with POST /packages")
	case JobRendition:
		return worker.Job{}, domain.Invalid("type", "renditions are encoded when a camera needs them; use %s", JobPrepareRenditions)
	}
	return worker.Job{}, domain.Invalid("type", "unknown job type %q", in.Type)
}

// CancelJob stops a job.
func (s *Service) CancelJob(ctx context.Context, actor Actor, id string) (worker.Job, error) {
	j, err := s.jobs.Cancel(ctx, id)
	if err != nil {
		return j, err
	}
	s.audit(ctx, actor, "job.cancel", "job", id, map[string]any{"name": j.Title, "type": j.Type})
	return j, nil
}

// ResumeJob queues an interrupted job again (D74).
func (s *Service) ResumeJob(ctx context.Context, actor Actor, id string) (worker.Job, error) {
	j, err := s.jobs.Resume(ctx, id)
	if err != nil {
		return j, err
	}
	s.audit(ctx, actor, "job.resume", "job", id, map[string]any{"name": j.Title, "type": j.Type})
	return j, nil
}

// AnswerJob answers the question a job waits on.
func (s *Service) AnswerJob(ctx context.Context, actor Actor, id, answer string) (worker.Job, error) {
	j, err := s.jobs.Answer(ctx, id, answer)
	if err != nil {
		return j, err
	}
	diff := map[string]any{"name": j.Title, "type": j.Type, "answer": answer}
	if j.Question != nil {
		diff["question"] = j.Question.Text
	}
	s.audit(ctx, actor, "job.answer", "job", id, diff)
	return j, nil
}

// WaitJob waits until a job stops running.
func (s *Service) WaitJob(ctx context.Context, id string) (worker.Job, error) {
	return s.jobs.Wait(ctx, id)
}

// jobError carries a typed error through a job's result, so an API call
// that waited for the job answers as if it had done the work itself.
type jobError struct {
	Kind    string `json:"kind"` // validation, conflict or error
	Field   string `json:"field,omitempty"`
	Message string `json:"message"`
}

func describeError(err error) *jobError {
	var (
		verr *domain.ValidationError
		cerr *domain.ConflictError
		ierr *ImportError
	)
	switch {
	case errors.As(err, &verr) && len(verr.Fields) > 0:
		return &jobError{Kind: "validation", Field: verr.Fields[0].Field, Message: verr.Fields[0].Message}
	case errors.As(err, &cerr):
		return &jobError{Kind: "conflict", Field: cerr.Field, Message: cerr.Message}
	case errors.As(err, &ierr):
		return &jobError{Kind: "rejected", Message: ierr.Error()}
	}
	return &jobError{Kind: "error", Message: err.Error()}
}

func (e *jobError) err() error {
	switch e.Kind {
	case "validation":
		return domain.Invalid(e.Field, "%s", e.Message)
	case "conflict":
		return domain.Conflict(e.Field, "%s", e.Message)
	}
	return errors.New(e.Message)
}
