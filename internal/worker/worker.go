// Package worker walks running sheets through their checklist. It is the only
// code that moves a sheet to success or failed.
package worker

import (
	"context"
	"log"
	"sync"
	"time"

	"listingboard/internal/board"
	"listingboard/internal/brochure"
)

type Worker struct {
	store   board.Store
	dataDir string
	pause   time.Duration
	poll    time.Duration

	mu     sync.Mutex
	active map[string]struct{}
	wg     sync.WaitGroup
}

// New returns a worker that waits pause between steps and looks for new
// running sheets every poll.
func New(store board.Store, dataDir string, pause, poll time.Duration) *Worker {
	return &Worker{
		store:   store,
		dataDir: dataDir,
		pause:   pause,
		poll:    poll,
		active:  map[string]struct{}{},
	}
}

// Run polls for running sheets until ctx is cancelled, then waits for the
// sheets in flight to stop. A stopped sheet is left in running so the next
// worker process continues it.
func (w *Worker) Run(ctx context.Context) error {
	ticker := time.NewTicker(w.poll)
	defer ticker.Stop()
	for {
		if err := w.pickUp(ctx); err != nil && ctx.Err() == nil {
			log.Printf("worker: list running sheets: %v", err)
		}
		select {
		case <-ctx.Done():
			w.wg.Wait()
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (w *Worker) pickUp(ctx context.Context) error {
	jobs, err := w.store.RunningJobs(ctx)
	if err != nil {
		return err
	}
	for _, job := range jobs {
		if !w.claim(job.ID) {
			continue
		}
		w.wg.Add(1)
		go func(id string) {
			defer w.wg.Done()
			defer w.release(id)
			if err := w.Process(ctx, id); err != nil && ctx.Err() == nil {
				log.Printf("worker: sheet %s: %v", id, err)
			}
		}(job.ID)
	}
	return nil
}

// Process walks one sheet from its first pending step until it is no longer
// running. Every pass re-reads the sheet, so a step that is already done is
// never done again and the runId chosen by the API is kept.
func (w *Worker) Process(ctx context.Context, id string) error {
	for {
		job, err := w.store.Job(ctx, id)
		if err != nil {
			return err
		}
		if job.Status != board.StatusRunning {
			return nil
		}
		now := time.Now().UTC()
		if err := brochure.Check(w.dataDir, job.UserID, job.ID); err != nil {
			job.Fail(board.ErrBrochureMissing, now)
			return w.store.UpdateJob(ctx, job)
		}
		job.Advance(now)
		if err := w.store.UpdateJob(ctx, job); err != nil {
			return err
		}
		if job.Status != board.StatusRunning {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(w.pause):
		}
	}
}

func (w *Worker) claim(id string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, busy := w.active[id]; busy {
		return false
	}
	w.active[id] = struct{}{}
	return true
}

func (w *Worker) release(id string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.active, id)
}
