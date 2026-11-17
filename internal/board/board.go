package board

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"time"
)

var (
	ErrNotFound  = errors.New("not found")
	ErrDuplicate = errors.New("duplicate")
	// ErrConflict means the requested state change is not allowed right now,
	// for example starting a second sheet while another one is running.
	ErrConflict = errors.New("conflict")
)

const (
	StatusDraft   = "draft"
	StatusRunning = "running"
	StatusSuccess = "success"
	StatusFailed  = "failed"

	StepPending = "pending"
	StepDone    = "done"

	// ErrBrochureMissing is the exact error text the spec requires on a sheet
	// whose brochure could not be read by the worker.
	ErrBrochureMissing = "Brochure is missing"
)

type User struct {
	ID           string
	Email        string
	PasswordHash string
}

type Step struct {
	Label string `json:"label"`
	State string `json:"state"`
}

type Job struct {
	ID        string    `json:"id"`
	UserID    string    `json:"-"`
	Title     string    `json:"title"`
	Address   string    `json:"address"`
	Status    string    `json:"status"`
	Error     string    `json:"error"`
	RunID     string    `json:"runId"`
	Filename  string    `json:"filename"`
	Checklist []Step    `json:"checklist"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// NextPendingStep returns the index of the first pending step, or -1 when
// every step is done.
func (j *Job) NextPendingStep() int {
	for i, step := range j.Checklist {
		if step.State == StepPending {
			return i
		}
	}
	return -1
}

// Advance marks the first pending step done. Once no step is pending the
// sheet becomes success in the same change, so a single save moves it out of
// running. A sheet whose steps were all done already just becomes success.
func (j *Job) Advance(now time.Time) {
	if i := j.NextPendingStep(); i >= 0 {
		j.Checklist[i].State = StepDone
	}
	if j.NextPendingStep() < 0 {
		j.Status = StatusSuccess
	}
	j.UpdatedAt = now
}

// Fail moves the sheet to failed with the given error text.
func (j *Job) Fail(message string, now time.Time) {
	j.Status = StatusFailed
	j.Error = message
	j.UpdatedAt = now
}

func NewChecklist() []Step {
	return []Step{
		{Label: "Reading the brochure", State: StepPending},
		{Label: "Checking the address", State: StepPending},
		{Label: "Publishing the sheet", State: StepPending},
	}
}

func NewID() string {
	return randomHex(16)
}

func NewToken() string {
	return randomHex(32)
}

func randomHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		panic(err)
	}
	return hex.EncodeToString(buf)
}

// Store is the persistence used by the HTTP API and the worker.
type Store interface {
	CreateUser(ctx context.Context, email, passwordHash string) (User, error)
	UserByEmail(ctx context.Context, email string) (User, error)
	CreateSession(ctx context.Context, userID, token string, expires time.Time) error
	DeleteSession(ctx context.Context, token string) error
	UserByToken(ctx context.Context, token string) (User, error)
	ListJobs(ctx context.Context, userID string) ([]Job, error)
	CreateJob(ctx context.Context, job Job) error
	Job(ctx context.Context, id string) (Job, error)
	UpdateJob(ctx context.Context, job Job) error
	// RunningJobs returns every sheet in running, across all accounts. The
	// worker uses it to pick up work.
	RunningJobs(ctx context.Context) ([]Job, error)
	// StartJob moves a draft to running with the given runId and returns the
	// updated sheet. It returns ErrNotFound when the sheet is no longer a
	// draft, and ErrConflict when the same account already has another sheet
	// in running.
	StartJob(ctx context.Context, id, runID string, at time.Time) (Job, error)
	// AttachBrochure records the client filename on a sheet that has none yet
	// and returns the updated sheet. It returns ErrNotFound when the sheet
	// already has a brochure, so of two racing uploads only one wins.
	AttachBrochure(ctx context.Context, id, filename string, at time.Time) (Job, error)
}
