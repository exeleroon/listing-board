package brochure

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"listingboard/internal/board"
	"listingboard/internal/memstore"
)

func TestDetectDecidesFromTheBytes(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		kind string
		err  error
	}{
		{"pdf", []byte("%PDF-1.7\n%\xe2\xe3\xcf\xd3\n"), KindPDF, nil},
		{"pdf header alone", []byte("%PDF-"), KindPDF, nil},
		{"ascii text", []byte("Two bedrooms, river view.\n"), KindText, nil},
		{"utf-8 text", []byte("Зимний сад — 120 m²"), KindText, nil},
		{"text of exactly the limit", bytes.Repeat([]byte("a"), MaxSize), KindText, nil},
		{"empty", []byte{}, "", ErrUnsupported},
		{"text with a NUL byte", []byte("hello\x00world"), "", ErrUnsupported},
		{"NUL beyond the first 512 bytes", append(bytes.Repeat([]byte("a"), 600), 0), "", ErrUnsupported},
		{"invalid utf-8", []byte("caf\xff"), "", ErrUnsupported},
		{"png", []byte("\x89PNG\r\n\x1a\n"), "", ErrUnsupported},
		{"pdf prefix in the middle only", []byte(" %PDF-1.4"), KindText, nil},
		{"one byte over the limit", bytes.Repeat([]byte("a"), MaxSize+1), "", ErrTooLarge},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kind, err := Detect(tc.data)
			if !errors.Is(err, tc.err) {
				t.Fatalf("err %v, want %v", err, tc.err)
			}
			if kind != tc.kind {
				t.Fatalf("kind %q, want %q", kind, tc.kind)
			}
		})
	}
}

func TestPathIgnoresTheClientFilename(t *testing.T) {
	got := Path("data", "user1", "job1")
	want := filepath.Join("data", "user1", "job1", "body")
	if got != want {
		t.Fatalf("path %q, want %q", got, want)
	}
}

func TestSaveReadCheck(t *testing.T) {
	dir := t.TempDir()
	if err := Check(dir, "u", "j"); err == nil {
		t.Fatal("Check should fail before anything is saved")
	}
	data := []byte("%PDF-1.4\n")
	if err := Save(dir, "u", "j", data); err != nil {
		t.Fatal(err)
	}
	got, err := Read(dir, "u", "j")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("read %q, want %q", got, data)
	}
	if err := Check(dir, "u", "j"); err != nil {
		t.Fatalf("Check after Save: %v", err)
	}
	if _, err := Read(dir, "u", "missing"); err == nil {
		t.Fatal("reading a missing brochure should fail")
	}
}

func TestStageAndCommit(t *testing.T) {
	dir := t.TempDir()
	first, err := Stage(dir, "u", "j", []byte("first"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := Stage(dir, "u", "j", []byte("second"))
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("two staged files share a name")
	}
	if err := Check(dir, "u", "j"); err == nil {
		t.Fatal("staging alone must not put a brochure in place")
	}
	if err := Commit(second, dir, "u", "j"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(first); err != nil {
		t.Fatal(err)
	}
	got, err := Read(dir, "u", "j")
	if err != nil || string(got) != "second" {
		t.Fatalf("read %q, %v; want the committed bytes", got, err)
	}
	entries, err := os.ReadDir(filepath.Dir(Path(dir, "u", "j")))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "body" {
		t.Fatalf("job dir holds %d entries, want only body", len(entries))
	}
}

func TestMigrateLegacyLayout(t *testing.T) {
	dir := t.TempDir()
	st := memstore.New()
	ctx := context.Background()
	legacy := seedJob(t, st, "owner-1", "brochure.pdf")
	current := seedJob(t, st, "owner-2", "flyer.txt")
	if err := os.MkdirAll(filepath.Join(dir, legacy.ID), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, legacy.ID, "brochure.pdf"), []byte("%PDF-old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Save(dir, current.UserID, current.ID, []byte("already here")); err != nil {
		t.Fatal(err)
	}
	orphan := filepath.Join(dir, "no-such-sheet", "stray.txt")
	if err := os.MkdirAll(filepath.Dir(orphan), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(orphan, []byte("stray"), 0o644); err != nil {
		t.Fatal(err)
	}

	moved, err := MigrateLegacyLayout(ctx, st, dir)
	if err != nil {
		t.Fatal(err)
	}
	if moved != 1 {
		t.Fatalf("moved %d, want 1", moved)
	}
	if got, err := Read(dir, legacy.UserID, legacy.ID); err != nil || string(got) != "%PDF-old" {
		t.Fatalf("legacy brochure not in place: %q %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(dir, legacy.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy dir still there: %v", err)
	}
	if got, _ := Read(dir, current.UserID, current.ID); string(got) != "already here" {
		t.Fatalf("current-layout brochure was touched: %q", got)
	}
	if _, err := os.Stat(orphan); err != nil {
		t.Fatalf("orphan file should be left alone: %v", err)
	}

	moved, err = MigrateLegacyLayout(ctx, st, dir)
	if err != nil || moved != 0 {
		t.Fatalf("second run moved %d, %v; want a no-op", moved, err)
	}
	if moved, err := MigrateLegacyLayout(ctx, st, filepath.Join(dir, "missing")); err != nil || moved != 0 {
		t.Fatalf("missing data dir: moved %d, %v", moved, err)
	}
}

func seedJob(t *testing.T, st *memstore.Store, userID, filename string) board.Job {
	t.Helper()
	job := board.Job{ID: board.NewID(), UserID: userID, Status: board.StatusDraft, Filename: filename, Checklist: board.NewChecklist()}
	if err := st.CreateJob(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	return job
}
