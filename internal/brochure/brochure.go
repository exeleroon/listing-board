// Package brochure decides what an uploaded brochure is and where its bytes
// live on disk. Both the API (write) and the worker (read) go through it.
package brochure

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"unicode/utf8"

	"listingboard/internal/board"
)

// MaxSize is the largest brochure the API accepts, in bytes.
const MaxSize = 1 << 20

const (
	KindPDF  = "pdf"
	KindText = "text"

	bodyName = "body"
)

var (
	ErrTooLarge    = errors.New("brochure is larger than 1 MiB")
	ErrUnsupported = errors.New("brochure is neither a PDF nor a text file")
)

var pdfMagic = []byte("%PDF-")

// Detect classifies the bytes. The multipart Content-Type and the client
// filename are deliberately not consulted.
func Detect(data []byte) (string, error) {
	if len(data) > MaxSize {
		return "", ErrTooLarge
	}
	if bytes.HasPrefix(data, pdfMagic) {
		return KindPDF, nil
	}
	if len(data) > 0 && utf8.Valid(data) && bytes.IndexByte(data, 0) < 0 {
		return KindText, nil
	}
	return "", ErrUnsupported
}

// ContentType is the MIME type to serve a brochure of the given kind with.
func ContentType(kind string) string {
	switch kind {
	case KindPDF:
		return "application/pdf"
	case KindText:
		return "text/plain; charset=utf-8"
	default:
		return "application/octet-stream"
	}
}

// Path is {dataDir}/{userID}/{jobID}/body. The client filename never becomes
// part of the path, so a name like ../../escaped.txt cannot leave dataDir.
func Path(dataDir, userID, jobID string) string {
	return filepath.Join(dataDir, userID, jobID, bodyName)
}

// Save writes the bytes straight to Path, creating the directories as needed.
func Save(dataDir, userID, jobID string, data []byte) error {
	path := Path(dataDir, userID, jobID)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// Stage writes the bytes to a uniquely named file next to Path and returns
// that file. Two uploads racing for one sheet therefore never touch each
// other's bytes; the one that wins the sheet calls Commit, the other removes
// its staged file.
func Stage(dataDir, userID, jobID string, data []byte) (string, error) {
	path := Path(dataDir, userID, jobID)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(filepath.Dir(path), bodyName+".*.tmp")
	if err != nil {
		return "", err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return "", err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

// Commit moves a staged file into place as the sheet's brochure.
func Commit(staged, dataDir, userID, jobID string) error {
	return os.Rename(staged, Path(dataDir, userID, jobID))
}

// Check reports whether the stored brochure can be opened for reading. It is
// what the worker asks before every step.
func Check(dataDir, userID, jobID string) error {
	f, err := os.Open(Path(dataDir, userID, jobID))
	if err != nil {
		return err
	}
	return f.Close()
}

// Read returns the stored bytes, or an error when there is nothing to read.
func Read(dataDir, userID, jobID string) ([]byte, error) {
	return os.ReadFile(Path(dataDir, userID, jobID))
}

// Lookup resolves a sheet by id; MigrateLegacyLayout needs the owner of a
// sheet to know where its brochure belongs now.
type Lookup interface {
	Job(ctx context.Context, id string) (board.Job, error)
}

// MigrateLegacyLayout moves brochures stored by the previous version of the
// API, which kept them at {dataDir}/{jobID}/{client filename}, to Path. It
// is safe to run on every start: a sheet whose brochure is already in place,
// or that no longer exists, is left alone. It returns the number of files
// moved and the first error it ran into after finishing the walk.
func MigrateLegacyLayout(ctx context.Context, lookup Lookup, dataDir string) (int, error) {
	entries, err := os.ReadDir(dataDir)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	moved := 0
	var first error
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		n, err := migrateLegacyDir(ctx, lookup, dataDir, entry.Name())
		moved += n
		if err != nil && first == nil {
			first = err
		}
	}
	return moved, first
}

// migrateLegacyDir handles one {dataDir}/{jobID} directory. Directories of
// the current layout are {dataDir}/{userID} and hold only subdirectories, so
// they fall through the "regular file" filter untouched.
func migrateLegacyDir(ctx context.Context, lookup Lookup, dataDir, jobID string) (int, error) {
	dir := filepath.Join(dataDir, jobID)
	files, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}
	moved := 0
	for _, file := range files {
		if !file.Type().IsRegular() {
			continue
		}
		job, err := lookup.Job(ctx, jobID)
		if errors.Is(err, board.ErrNotFound) {
			return moved, nil
		}
		if err != nil {
			return moved, err
		}
		if filepath.Base(job.Filename) != file.Name() {
			continue
		}
		target := Path(dataDir, job.UserID, job.ID)
		if _, err := os.Stat(target); err == nil {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return moved, err
		}
		if err := os.Rename(filepath.Join(dir, file.Name()), target); err != nil {
			return moved, fmt.Errorf("move brochure of sheet %s: %w", job.ID, err)
		}
		moved++
		// Best effort: the old directory is empty now unless something else lives there.
		_ = os.Remove(dir)
	}
	return moved, nil
}
