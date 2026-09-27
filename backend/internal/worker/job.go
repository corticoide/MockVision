// Package worker runs MockVision's background jobs (D72, D74): encoding
// renditions and importing packages now, captures and backups later.
//
// A job's state, steps, checkpoint, pending question and events live in
// SQLite, so closing the browser stops nothing, and a node restart leaves
// the job Interrupted, to resume from its last checkpoint. At most a
// configurable number of jobs run at once; the rest wait in the queue.
package worker

import (
	"encoding/json"
	"time"

	"github.com/corticoide/mockvision/backend/internal/store"
	"github.com/corticoide/mockvision/backend/internal/store/db"
)

// Status of a job.
type Status string

// Job states. Queued jobs wait for a free slot; Waiting ones wait for an
// answer from the user; Interrupted ones stopped with the node and resume
// on request.
const (
	Queued      Status = "queued"
	Running     Status = "running"
	Waiting     Status = "waiting"
	Completed   Status = "completed"
	Failed      Status = "failed"
	Canceled    Status = "canceled"
	Interrupted Status = "interrupted"
)

// Final reports whether the job is over for good.
func (s Status) Final() bool { return s == Completed || s == Failed || s == Canceled }

// Busy reports whether the job holds or awaits a slot of its own accord.
func (s Status) Busy() bool { return s == Queued || s == Running || s == Waiting }

// Option is a possible answer to a question.
type Option struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

// Question is a decision a job asks the user for. Without an answer before
// ExpiresAt the job goes on with Default, or is canceled when there is none.
type Question struct {
	ID        string    `json:"id"`
	Text      string    `json:"text"`
	Options   []Option  `json:"options"`
	Default   string    `json:"default"`
	ExpiresAt time.Time `json:"expires_at"`
}

// Job is a background job as the API returns it. Its parameters and
// checkpoint stay internal: they may hold paths and credentials.
type Job struct {
	ID         string          `json:"id"`
	Type       string          `json:"type"`
	Title      string          `json:"title"`
	Status     Status          `json:"status"`
	Progress   float64         `json:"progress"`
	Step       string          `json:"step"`
	Question   *Question       `json:"question"`
	Result     json.RawMessage `json:"result"`
	Error      string          `json:"error"`
	CreatedBy  string          `json:"created_by"`
	CreatedAt  time.Time       `json:"created_at"`
	StartedAt  *time.Time      `json:"started_at"`
	FinishedAt *time.Time      `json:"finished_at"`

	params     json.RawMessage
	checkpoint json.RawMessage
}

// Event kinds of a job's history.
const (
	EventStatus   = "status"
	EventStep     = "step"
	EventLog      = "log"
	EventQuestion = "question"
	EventAnswer   = "answer"
)

// Event is one entry of a job's history.
type Event struct {
	JobID string          `json:"job_id"`
	Seq   int64           `json:"seq"`
	At    time.Time       `json:"at"`
	Kind  string          `json:"kind"`
	Data  json.RawMessage `json:"data"`
}

// DecodeParams decodes the job's parameters, for code outside a run such
// as a kind's Cleanup.
func (j Job) DecodeParams(v any) error { return json.Unmarshal(j.params, v) }

func jobFrom(r db.Job) Job {
	j := Job{
		ID: r.ID, Type: r.Type, Title: r.Title, Status: Status(r.Status), Progress: r.Progress, Step: r.Step,
		Result: json.RawMessage(r.ResultJson), Error: r.Error, CreatedBy: r.CreatedBy, CreatedAt: store.Time(r.CreatedAt),
		params: json.RawMessage(r.ParamsJson), checkpoint: json.RawMessage(r.CheckpointJson),
	}
	if !json.Valid(j.Result) {
		j.Result = json.RawMessage("{}")
	}
	if r.QuestionJson.Valid {
		var q Question
		if json.Unmarshal([]byte(r.QuestionJson.String), &q) == nil {
			j.Question = &q
		}
	}
	if t := store.NullTime(r.StartedAt); !t.IsZero() {
		j.StartedAt = &t
	}
	if t := store.NullTime(r.FinishedAt); !t.IsZero() {
		j.FinishedAt = &t
	}
	return j
}

func (j *Job) update() db.UpdateJobParams {
	p := db.UpdateJobParams{
		ID: j.ID, Status: string(j.Status), Progress: j.Progress, Step: j.Step, CheckpointJson: string(j.checkpoint),
		ResultJson: string(j.Result), Error: j.Error,
	}
	if len(j.checkpoint) == 0 {
		p.CheckpointJson = "{}"
	}
	if len(j.Result) == 0 {
		p.ResultJson = "{}"
	}
	if j.Question != nil {
		raw, _ := json.Marshal(j.Question)
		p.QuestionJson.String, p.QuestionJson.Valid = string(raw), true
	}
	if j.StartedAt != nil {
		p.StartedAt = store.NullMillis(*j.StartedAt)
	}
	if j.FinishedAt != nil {
		p.FinishedAt = store.NullMillis(*j.FinishedAt)
	}
	return p
}

func eventFrom(r db.JobEvent) Event {
	return Event{JobID: r.JobID, Seq: r.Seq, At: store.Time(r.At), Kind: r.Kind, Data: json.RawMessage(r.PayloadJson)}
}
