// Package memstore is an in-memory board.Store for tests. It needs no
// Postgres and is shared by the HTTP API and worker test suites.
package memstore

import (
	"context"
	"sync"
	"time"

	"listingboard/internal/board"
)

type Store struct {
	mu       sync.Mutex
	users    map[string]board.User
	byEmail  map[string]string
	sessions map[string]session
	jobs     map[string]board.Job
}

type session struct {
	userID  string
	expires time.Time
}

func New() *Store {
	return &Store{
		users:    map[string]board.User{},
		byEmail:  map[string]string{},
		sessions: map[string]session{},
		jobs:     map[string]board.Job{},
	}
}

func (m *Store) CreateUser(_ context.Context, email, passwordHash string) (board.User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.byEmail[email]; ok {
		return board.User{}, board.ErrDuplicate
	}
	user := board.User{ID: board.NewID(), Email: email, PasswordHash: passwordHash}
	m.users[user.ID] = user
	m.byEmail[email] = user.ID
	return user, nil
}

func (m *Store) UserByEmail(_ context.Context, email string) (board.User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id, ok := m.byEmail[email]
	if !ok {
		return board.User{}, board.ErrNotFound
	}
	return m.users[id], nil
}

func (m *Store) CreateSession(_ context.Context, userID, token string, expires time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessions[token] = session{userID: userID, expires: expires}
	return nil
}

func (m *Store) DeleteSession(_ context.Context, token string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, token)
	return nil
}

func (m *Store) UserByToken(_ context.Context, token string) (board.User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[token]
	if !ok || time.Now().After(s.expires) {
		return board.User{}, board.ErrNotFound
	}
	user, ok := m.users[s.userID]
	if !ok {
		return board.User{}, board.ErrNotFound
	}
	return user, nil
}

func (m *Store) ListJobs(_ context.Context, userID string) ([]board.Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.filter(func(job board.Job) bool { return job.UserID == userID }), nil
}

func (m *Store) RunningJobs(_ context.Context) ([]board.Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.filter(func(job board.Job) bool { return job.Status == board.StatusRunning }), nil
}

func (m *Store) CreateJob(_ context.Context, job board.Job) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.jobs[job.ID] = clone(job)
	return nil
}

func (m *Store) Job(_ context.Context, id string) (board.Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	job, ok := m.jobs[id]
	if !ok {
		return board.Job{}, board.ErrNotFound
	}
	return clone(job), nil
}

func (m *Store) UpdateJob(_ context.Context, job board.Job) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.jobs[job.ID]; !ok {
		return board.ErrNotFound
	}
	m.jobs[job.ID] = clone(job)
	return nil
}

func (m *Store) StartJob(_ context.Context, id, runID string, at time.Time) (board.Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	job, ok := m.jobs[id]
	if !ok || job.Status != board.StatusDraft {
		return board.Job{}, board.ErrNotFound
	}
	for _, other := range m.jobs {
		if other.UserID == job.UserID && other.Status == board.StatusRunning {
			return board.Job{}, board.ErrConflict
		}
	}
	job.Status = board.StatusRunning
	job.RunID = runID
	job.Error = ""
	job.UpdatedAt = at
	m.jobs[id] = clone(job)
	return clone(job), nil
}

func (m *Store) AttachBrochure(_ context.Context, id, filename string, at time.Time) (board.Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	job, ok := m.jobs[id]
	if !ok || job.Filename != "" {
		return board.Job{}, board.ErrNotFound
	}
	job.Filename = filename
	job.UpdatedAt = at
	m.jobs[id] = clone(job)
	return clone(job), nil
}

// filter must be called with the lock held.
func (m *Store) filter(keep func(board.Job) bool) []board.Job {
	jobs := []board.Job{}
	for _, job := range m.jobs {
		if keep(job) {
			jobs = append(jobs, clone(job))
		}
	}
	return jobs
}

func clone(job board.Job) board.Job {
	job.Checklist = append([]board.Step(nil), job.Checklist...)
	return job
}
