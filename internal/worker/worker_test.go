package worker

import (
	"context"
	"sync"
	"testing"
	"time"

	"listingboard/internal/board"
	"listingboard/internal/brochure"
	"listingboard/internal/memstore"
)

func TestProcessWalksTheChecklistOneStepPerSave(t *testing.T) {
	st, dir := newStore(t)
	job := seedJob(t, st, dir, board.StatusRunning, "run-1", board.NewChecklist(), true)
	rec := &recorder{Store: st}

	if err := New(rec, dir, 0, time.Millisecond).Process(context.Background(), job.ID); err != nil {
		t.Fatal(err)
	}

	got := load(t, st, job.ID)
	if got.Status != board.StatusSuccess || got.Error != "" {
		t.Fatalf("status %q error %q, want success with no error", got.Status, got.Error)
	}
	if got.RunID != "run-1" {
		t.Fatalf("runId changed to %q", got.RunID)
	}
	saves := rec.snapshot()
	if len(saves) != 3 {
		t.Fatalf("%d saves, want one per step", len(saves))
	}
	for i, saved := range saves {
		if n := doneSteps(saved); n != i+1 {
			t.Fatalf("save %d has %d done steps, want %d", i+1, n, i+1)
		}
		wantStatus := board.StatusRunning
		if i == len(saves)-1 {
			wantStatus = board.StatusSuccess
		}
		if saved.Status != wantStatus {
			t.Fatalf("save %d status %q, want %q", i+1, saved.Status, wantStatus)
		}
	}
	for _, step := range got.Checklist {
		if step.State != board.StepDone {
			t.Fatalf("step %q is %s", step.Label, step.State)
		}
	}
}

func TestProcessContinuesFromTheFirstPendingStep(t *testing.T) {
	st, dir := newStore(t)
	steps := board.NewChecklist()
	steps[0].State = board.StepDone
	job := seedJob(t, st, dir, board.StatusRunning, "run-2", steps, true)
	rec := &recorder{Store: st}

	if err := New(rec, dir, 0, time.Millisecond).Process(context.Background(), job.ID); err != nil {
		t.Fatal(err)
	}

	saves := rec.snapshot()
	if len(saves) != 2 {
		t.Fatalf("%d saves, want 2: the done step must not be done again", len(saves))
	}
	if doneSteps(saves[0]) != 2 || saves[0].Checklist[1].State != board.StepDone {
		t.Fatalf("first save should complete the second step, got %+v", saves[0].Checklist)
	}
	got := load(t, st, job.ID)
	if got.Status != board.StatusSuccess || got.RunID != "run-2" {
		t.Fatalf("status %q runId %q", got.Status, got.RunID)
	}
}

func TestProcessWaitsBetweenSteps(t *testing.T) {
	st, dir := newStore(t)
	job := seedJob(t, st, dir, board.StatusRunning, "run-3", board.NewChecklist(), true)
	rec := &recorder{Store: st}
	pause := 40 * time.Millisecond

	started := time.Now()
	if err := New(rec, dir, pause, time.Millisecond).Process(context.Background(), job.ID); err != nil {
		t.Fatal(err)
	}
	// Three steps, two pauses between them, none after the last.
	if elapsed := time.Since(started); elapsed < 2*pause {
		t.Fatalf("finished in %s, want at least %s", elapsed, 2*pause)
	}
	if len(rec.snapshot()) != 3 {
		t.Fatalf("%d saves, want 3", len(rec.snapshot()))
	}
}

func TestProcessFailsWhenTheBrochureIsMissing(t *testing.T) {
	st, dir := newStore(t)
	job := seedJob(t, st, dir, board.StatusRunning, "run-4", board.NewChecklist(), false)

	if err := New(st, dir, 0, time.Millisecond).Process(context.Background(), job.ID); err != nil {
		t.Fatal(err)
	}

	got := load(t, st, job.ID)
	if got.Status != board.StatusFailed {
		t.Fatalf("status %q, want failed", got.Status)
	}
	if got.Error != "Brochure is missing" {
		t.Fatalf("error %q, want %q", got.Error, "Brochure is missing")
	}
	if got.RunID != "run-4" {
		t.Fatalf("runId changed to %q", got.RunID)
	}
	for _, step := range got.Checklist {
		if step.State != board.StepPending {
			t.Fatalf("step %q is %s, want pending", step.Label, step.State)
		}
	}
}

func TestProcessLeavesSheetsThatAreNotRunningAlone(t *testing.T) {
	st, dir := newStore(t)
	rec := &recorder{Store: st}
	w := New(rec, dir, 0, time.Millisecond)
	for _, status := range []string{board.StatusDraft, board.StatusSuccess, board.StatusFailed} {
		job := seedJob(t, st, dir, status, "", board.NewChecklist(), true)
		if err := w.Process(context.Background(), job.ID); err != nil {
			t.Fatal(err)
		}
		if got := load(t, st, job.ID); got.Status != status {
			t.Fatalf("%s sheet became %s", status, got.Status)
		}
	}
	if n := len(rec.snapshot()); n != 0 {
		t.Fatalf("%d saves on sheets that are not running", n)
	}
}

func TestStoppedWorkerLeavesTheSheetRunningAndTheNextOneContinues(t *testing.T) {
	st, dir := newStore(t)
	job := seedJob(t, st, dir, board.StatusRunning, "run-5", board.NewChecklist(), true)

	// Kill the first worker right after its first save.
	ctx, cancel := context.WithCancel(context.Background())
	first := &recorder{Store: st, afterSave: cancel}
	err := New(first, dir, time.Hour, time.Millisecond).Process(ctx, job.ID)
	if err == nil || ctx.Err() == nil {
		t.Fatalf("expected the cancelled context to stop the worker, got %v", err)
	}
	mid := load(t, st, job.ID)
	if mid.Status != board.StatusRunning || doneSteps(mid) != 1 || mid.RunID != "run-5" {
		t.Fatalf("after kill: status %q, %d done, runId %q", mid.Status, doneSteps(mid), mid.RunID)
	}

	second := &recorder{Store: st}
	if err := New(second, dir, 0, time.Millisecond).Process(context.Background(), job.ID); err != nil {
		t.Fatal(err)
	}
	if n := len(second.snapshot()); n != 2 {
		t.Fatalf("second worker made %d saves, want 2 (the done step stays done)", n)
	}
	got := load(t, st, job.ID)
	if got.Status != board.StatusSuccess || got.RunID != "run-5" {
		t.Fatalf("status %q runId %q, want success with the original runId", got.Status, got.RunID)
	}
}

func TestRunPicksUpEveryRunningSheet(t *testing.T) {
	st, dir := newStore(t)
	a := seedJob(t, st, dir, board.StatusRunning, "run-a", board.NewChecklist(), true)
	b := seedJob(t, st, dir, board.StatusRunning, "run-b", board.NewChecklist(), true)
	draft := seedJob(t, st, dir, board.StatusDraft, "", board.NewChecklist(), true)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- New(st, dir, 0, 5*time.Millisecond).Run(ctx) }()

	deadline := time.Now().Add(3 * time.Second)
	for load(t, st, a.ID).Status != board.StatusSuccess || load(t, st, b.ID).Status != board.StatusSuccess {
		if time.Now().After(deadline) {
			t.Fatalf("sheets not finished: a=%s b=%s", load(t, st, a.ID).Status, load(t, st, b.ID).Status)
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != context.Canceled {
		t.Fatalf("Run returned %v, want context.Canceled", err)
	}
	if got := load(t, st, draft.ID); got.Status != board.StatusDraft {
		t.Fatalf("draft became %s", got.Status)
	}
}

// recorder keeps a copy of every saved sheet so a test can see what each
// step wrote, and can stop the worker right after a save.
type recorder struct {
	board.Store
	mu        sync.Mutex
	saves     []board.Job
	afterSave func()
}

func (r *recorder) UpdateJob(ctx context.Context, job board.Job) error {
	if err := r.Store.UpdateJob(ctx, job); err != nil {
		return err
	}
	job.Checklist = append([]board.Step(nil), job.Checklist...)
	r.mu.Lock()
	r.saves = append(r.saves, job)
	r.mu.Unlock()
	if r.afterSave != nil {
		r.afterSave()
	}
	return nil
}

func (r *recorder) snapshot() []board.Job {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]board.Job(nil), r.saves...)
}

func newStore(t *testing.T) (*memstore.Store, string) {
	t.Helper()
	return memstore.New(), t.TempDir()
}

func seedJob(t *testing.T, st board.Store, dir, status, runID string, steps []board.Step, withBrochure bool) board.Job {
	t.Helper()
	now := time.Now().UTC()
	job := board.Job{
		ID:        board.NewID(),
		UserID:    board.NewID(),
		Title:     "Market loft",
		Address:   "10 Market Street",
		Status:    status,
		RunID:     runID,
		Filename:  "brochure.pdf",
		Checklist: steps,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := st.CreateJob(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if withBrochure {
		if err := brochure.Save(dir, job.UserID, job.ID, []byte("%PDF-1.4\n")); err != nil {
			t.Fatal(err)
		}
	}
	return job
}

func load(t *testing.T, st board.Store, id string) board.Job {
	t.Helper()
	job, err := st.Job(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return job
}

func doneSteps(job board.Job) int {
	n := 0
	for _, step := range job.Checklist {
		if step.State == board.StepDone {
			n++
		}
	}
	return n
}
