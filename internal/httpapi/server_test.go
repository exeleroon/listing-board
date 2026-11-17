package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"listingboard/internal/board"
	"listingboard/internal/brochure"
	"listingboard/internal/memstore"
	"listingboard/internal/worker"
)

// Rewritten for the spec: the handler no longer finishes the sheet. POST /start
// hands it to the worker as running, with a fresh runId and every step still
// pending, and only the worker marks it successful, keeping that runId.
func TestStartMarksTheJobSuccessful(t *testing.T) {
	srv, dir := newTestServer(t)
	cookie := signUp(t, srv, "ada@example.com")
	job := createSheet(t, srv, cookie, "Market loft", "10 Market Street")
	uploadFile(t, srv, cookie, job.ID, "brochure.pdf", []byte("%PDF-1.4\n"), "application/pdf")

	res := call(srv, http.MethodPost, "/api/jobs/"+job.ID+"/start", cookie, nil, "")
	if res.Code != http.StatusOK {
		t.Fatalf("status %d: %s", res.Code, res.Body.String())
	}
	got := decodeJob(t, res)
	if got.Status != board.StatusRunning {
		t.Fatalf("status %s, want running", got.Status)
	}
	if got.RunID == "" {
		t.Fatal("runId is empty")
	}
	if got.Error != "" {
		t.Fatalf("error %q, want empty", got.Error)
	}
	for _, step := range got.Checklist {
		if step.State != board.StepPending {
			t.Fatalf("step %q is %s, want pending", step.Label, step.State)
		}
	}
	stored := getSheet(t, srv, cookie, job.ID)
	if stored.Status != board.StatusRunning || stored.RunID != got.RunID {
		t.Fatalf("stored sheet is %s with runId %q, want running with %q", stored.Status, stored.RunID, got.RunID)
	}

	// The worker, not the handler, walks the checklist and finishes the sheet.
	if err := worker.New(srv.store, dir, 0, time.Millisecond).Process(context.Background(), job.ID); err != nil {
		t.Fatal(err)
	}
	done := getSheet(t, srv, cookie, job.ID)
	if done.Status != board.StatusSuccess {
		t.Fatalf("status %s after the worker, want success", done.Status)
	}
	if done.RunID != got.RunID {
		t.Fatalf("runId changed from %q to %q", got.RunID, done.RunID)
	}
	for _, step := range done.Checklist {
		if step.State != board.StepDone {
			t.Fatalf("step %q is %s, want done", step.Label, step.State)
		}
	}
}

func TestStartAgainReturnsTheSameRunID(t *testing.T) {
	srv, _ := newTestServer(t)
	cookie := signUp(t, srv, "ada@example.com")
	job := createSheet(t, srv, cookie, "Market loft", "10 Market Street")
	uploadFile(t, srv, cookie, job.ID, "brochure.pdf", []byte("%PDF-1.4\n"), "application/pdf")

	first := decodeJob(t, mustCall(t, srv, http.MethodPost, "/api/jobs/"+job.ID+"/start", cookie, http.StatusOK))
	second := decodeJob(t, mustCall(t, srv, http.MethodPost, "/api/jobs/"+job.ID+"/start", cookie, http.StatusOK))
	if second.RunID != first.RunID {
		t.Fatalf("second start changed runId from %q to %q", first.RunID, second.RunID)
	}
	if second.Status != board.StatusRunning {
		t.Fatalf("status %s, want running", second.Status)
	}
}

func TestStartWithoutABrochure(t *testing.T) {
	srv, _ := newTestServer(t)
	cookie := signUp(t, srv, "ada@example.com")
	job := createSheet(t, srv, cookie, "Market loft", "10 Market Street")

	res := mustCall(t, srv, http.MethodPost, "/api/jobs/"+job.ID+"/start", cookie, http.StatusBadRequest)
	if msg := errorMessage(t, res); msg != "Attach a brochure first" {
		t.Fatalf("error %q, want %q", msg, "Attach a brochure first")
	}
	if got := getSheet(t, srv, cookie, job.ID); got.Status != board.StatusDraft || got.RunID != "" {
		t.Fatalf("sheet is %s with runId %q, want an untouched draft", got.Status, got.RunID)
	}
}

func TestStartRejectsASecondRunningSheet(t *testing.T) {
	srv, _ := newTestServer(t)
	cookie := signUp(t, srv, "ada@example.com")
	first := createSheet(t, srv, cookie, "Market loft", "10 Market Street")
	second := createSheet(t, srv, cookie, "River flat", "2 Quay Road")
	uploadFile(t, srv, cookie, first.ID, "a.pdf", []byte("%PDF-1.4\n"), "application/pdf")
	uploadFile(t, srv, cookie, second.ID, "b.pdf", []byte("%PDF-1.4\n"), "application/pdf")

	mustCall(t, srv, http.MethodPost, "/api/jobs/"+first.ID+"/start", cookie, http.StatusOK)
	mustCall(t, srv, http.MethodPost, "/api/jobs/"+second.ID+"/start", cookie, http.StatusConflict)

	if got := getSheet(t, srv, cookie, second.ID); got.Status != board.StatusDraft || got.RunID != "" {
		t.Fatalf("second sheet is %s with runId %q, want an untouched draft", got.Status, got.RunID)
	}
	if got := getSheet(t, srv, cookie, first.ID); got.Status != board.StatusRunning {
		t.Fatalf("first sheet is %s, want running", got.Status)
	}
}

func TestStartLeavesAFailedSheetFailed(t *testing.T) {
	srv, _ := newTestServer(t)
	cookie := signUp(t, srv, "ada@example.com")
	job := createSheet(t, srv, cookie, "Market loft", "10 Market Street")
	uploadFile(t, srv, cookie, job.ID, "brochure.pdf", []byte("%PDF-1.4\n"), "application/pdf")
	setStatus(t, srv, job.ID, board.StatusFailed, "Brochure is missing")

	mustCall(t, srv, http.MethodPost, "/api/jobs/"+job.ID+"/start", cookie, http.StatusConflict)

	got := getSheet(t, srv, cookie, job.ID)
	if got.Status != board.StatusFailed || got.Error != "Brochure is missing" {
		t.Fatalf("sheet is %s with error %q, want failed with the original error", got.Status, got.Error)
	}
}

func TestSheetOfAnotherAccountIsNotFound(t *testing.T) {
	srv, _ := newTestServer(t)
	ada := signUp(t, srv, "ada@example.com")
	grace := signUp(t, srv, "grace@example.com")
	job := createSheet(t, srv, ada, "Market loft", "10 Market Street")
	uploadFile(t, srv, ada, job.ID, "brochure.pdf", []byte("%PDF-1.4\n"), "application/pdf")

	mustCall(t, srv, http.MethodGet, "/api/jobs/"+job.ID, grace, http.StatusNotFound)
	mustCall(t, srv, http.MethodPost, "/api/jobs/"+job.ID+"/start", grace, http.StatusNotFound)
	mustCall(t, srv, http.MethodGet, "/api/jobs/"+job.ID+"/file", grace, http.StatusNotFound)
	if res := tryUpload(srv, grace, job.ID, "x.txt", []byte("hello"), "text/plain"); res.Code != http.StatusNotFound {
		t.Fatalf("upload by another account: %d %s", res.Code, res.Body.String())
	}
	mustCall(t, srv, http.MethodGet, "/api/jobs/"+board.NewID(), ada, http.StatusNotFound)

	if got := getSheet(t, srv, ada, job.ID); got.Status != board.StatusDraft {
		t.Fatalf("owner sees %s, want draft", got.Status)
	}
}

// Rewritten for the spec: the client name is kept in the sheet's filename
// field only; the bytes live at {DATA_DIR}/{userID}/{jobID}/body and nowhere else.
func TestUploadKeepsTheClientFilename(t *testing.T) {
	srv, dir := newTestServer(t)
	cookie := signUp(t, srv, "ada@example.com")
	me := whoAmI(t, srv, cookie)
	job := createSheet(t, srv, cookie, "Market loft", "10 Market Street")
	data := []byte("%PDF-1.4\n")
	uploadFile(t, srv, cookie, job.ID, "brochure.pdf", data, "application/pdf")

	path := filepath.Join(dir, me.ID, job.ID, "body")
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("brochure was not stored at %s: %v", path, err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("stored %q, want %q", got, data)
	}
	if files := filesUnder(t, dir); len(files) != 1 || files[0] != path {
		t.Fatalf("files under data dir: %v, want only %s", files, path)
	}
	if sheet := getSheet(t, srv, cookie, job.ID); sheet.Filename != "brochure.pdf" {
		t.Fatalf("filename %q, want brochure.pdf", sheet.Filename)
	}
}

func TestUploadWithAnEscapingNameStaysInsideTheDataDir(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "a", "b", "data")
	srv := New(memstore.New(), dir, "")
	cookie := signUp(t, srv, "ada@example.com")
	me := whoAmI(t, srv, cookie)
	job := createSheet(t, srv, cookie, "Market loft", "10 Market Street")
	uploadFile(t, srv, cookie, job.ID, "../../escaped.txt", []byte("hello"), "text/plain")

	if sheet := getSheet(t, srv, cookie, job.ID); sheet.Filename != "../../escaped.txt" {
		t.Fatalf("filename %q, want the original name", sheet.Filename)
	}
	want := filepath.Join(dir, me.ID, job.ID, "body")
	if files := filesUnder(t, root); len(files) != 1 || files[0] != want {
		t.Fatalf("files written: %v, want only %s", files, want)
	}

	res := mustCall(t, srv, http.MethodGet, "/api/jobs/"+job.ID+"/file", cookie, http.StatusOK)
	if res.Body.String() != "hello" {
		t.Fatalf("download body %q", res.Body.String())
	}
	if cd := res.Header().Get("Content-Disposition"); !strings.Contains(cd, `"escaped.txt"`) || strings.Contains(cd, "..") {
		t.Fatalf("Content-Disposition %q", cd)
	}
}

func TestUploadRejectsASecondFile(t *testing.T) {
	srv, _ := newTestServer(t)
	cookie := signUp(t, srv, "ada@example.com")
	job := createSheet(t, srv, cookie, "Market loft", "10 Market Street")
	uploadFile(t, srv, cookie, job.ID, "first.txt", []byte("first"), "text/plain")

	if res := tryUpload(srv, cookie, job.ID, "second.txt", []byte("second"), "text/plain"); res.Code != http.StatusBadRequest {
		t.Fatalf("second upload: %d %s", res.Code, res.Body.String())
	}
	if sheet := getSheet(t, srv, cookie, job.ID); sheet.Filename != "first.txt" {
		t.Fatalf("filename %q, want first.txt", sheet.Filename)
	}
	if res := mustCall(t, srv, http.MethodGet, "/api/jobs/"+job.ID+"/file", cookie, http.StatusOK); res.Body.String() != "first" {
		t.Fatalf("download body %q, want the first file", res.Body.String())
	}
}

func TestConcurrentFirstUploadsKeepExactlyOneFile(t *testing.T) {
	names := []string{"one.txt", "two.txt"}
	for round := 0; round < 10; round++ {
		srv, dir := newTestServer(t)
		cookie := signUp(t, srv, "ada@example.com")
		me := whoAmI(t, srv, cookie)
		job := createSheet(t, srv, cookie, "Market loft", "10 Market Street")

		codes := make([]int, len(names))
		var wg sync.WaitGroup
		for i, name := range names {
			wg.Add(1)
			go func(i int, name string) {
				defer wg.Done()
				codes[i] = tryUpload(srv, cookie, job.ID, name, []byte(name), "text/plain").Code
			}(i, name)
		}
		wg.Wait()

		winner := -1
		for i, c := range codes {
			switch {
			case c == http.StatusOK && winner < 0:
				winner = i
			case c == http.StatusOK:
				t.Fatalf("round %d: both uploads succeeded: %v", round, codes)
			case c != http.StatusBadRequest:
				t.Fatalf("round %d: unexpected status %v", round, codes)
			}
		}
		if winner < 0 {
			t.Fatalf("round %d: no upload succeeded: %v", round, codes)
		}
		if sheet := getSheet(t, srv, cookie, job.ID); sheet.Filename != names[winner] {
			t.Fatalf("round %d: filename %q, want the winner %q", round, sheet.Filename, names[winner])
		}
		path := filepath.Join(dir, me.ID, job.ID, "body")
		if files := filesUnder(t, dir); len(files) != 1 || files[0] != path {
			t.Fatalf("round %d: files on disk %v, want only %s", round, files, path)
		}
		if got, _ := os.ReadFile(path); string(got) != names[winner] {
			t.Fatalf("round %d: body holds %q, want the winner's bytes", round, got)
		}
	}
}

func TestUploadRejectsAnOverlongFilename(t *testing.T) {
	srv, dir := newTestServer(t)
	cookie := signUp(t, srv, "ada@example.com")
	job := createSheet(t, srv, cookie, "Market loft", "10 Market Street")

	res := tryUpload(srv, cookie, job.ID, strings.Repeat("a", 256)+".txt", []byte("hello"), "text/plain")
	if res.Code != http.StatusBadRequest {
		t.Fatalf("status %d: %s", res.Code, res.Body.String())
	}
	if files := filesUnder(t, dir); len(files) != 0 {
		t.Fatalf("rejected upload left files on disk: %v", files)
	}
}

func TestDownloadNameDropsWindowsDirectories(t *testing.T) {
	srv, _ := newTestServer(t)
	cookie := signUp(t, srv, "ada@example.com")
	job := createSheet(t, srv, cookie, "Market loft", "10 Market Street")
	uploadFile(t, srv, cookie, job.ID, `..\..\escaped.txt`, []byte("hello"), "text/plain")

	if sheet := getSheet(t, srv, cookie, job.ID); sheet.Filename != `..\..\escaped.txt` {
		t.Fatalf("filename %q, want the original name", sheet.Filename)
	}
	res := mustCall(t, srv, http.MethodGet, "/api/jobs/"+job.ID+"/file", cookie, http.StatusOK)
	if cd := res.Header().Get("Content-Disposition"); !strings.Contains(cd, `"escaped.txt"`) || strings.Contains(cd, "..") {
		t.Fatalf("Content-Disposition %q", cd)
	}
}

func TestUploadDecidesTheTypeFromTheBytes(t *testing.T) {
	cases := []struct {
		name        string
		filename    string
		contentType string
		data        []byte
		want        int
	}{
		{"pdf bytes with a text name and type", "notes.txt", "text/plain", []byte("%PDF-1.4\n"), http.StatusOK},
		{"text bytes with a pdf name and type", "flyer.pdf", "application/pdf", []byte("Two bedrooms"), http.StatusOK},
		{"utf-8 text", "flyer.txt", "text/plain", []byte("Зимний сад"), http.StatusOK},
		{"empty file", "empty.txt", "text/plain", []byte{}, http.StatusBadRequest},
		{"png bytes with a pdf name", "flyer.pdf", "application/pdf", []byte("\x89PNG\r\n\x1a\n"), http.StatusBadRequest},
		{"text with a NUL byte", "flyer.txt", "text/plain", []byte("hello\x00"), http.StatusBadRequest},
		{"invalid utf-8", "flyer.txt", "text/plain", []byte("caf\xff"), http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, dir := newTestServer(t)
			cookie := signUp(t, srv, "ada@example.com")
			job := createSheet(t, srv, cookie, "Market loft", "10 Market Street")

			res := tryUpload(srv, cookie, job.ID, tc.filename, tc.data, tc.contentType)
			if res.Code != tc.want {
				t.Fatalf("status %d, want %d: %s", res.Code, tc.want, res.Body.String())
			}
			files := filesUnder(t, dir)
			if tc.want == http.StatusOK && len(files) != 1 {
				t.Fatalf("files on disk: %v, want one", files)
			}
			if tc.want != http.StatusOK && len(files) != 0 {
				t.Fatalf("rejected upload left files on disk: %v", files)
			}
			if sheet := getSheet(t, srv, cookie, job.ID); (sheet.Filename != "") != (tc.want == http.StatusOK) {
				t.Fatalf("filename %q after status %d", sheet.Filename, res.Code)
			}
		})
	}
}

func TestUploadRejectsFilesOverOneMiB(t *testing.T) {
	for _, size := range []int{brochure.MaxSize + 1, 5 << 20} {
		t.Run(fmt.Sprintf("%d bytes", size), func(t *testing.T) {
			srv, dir := newTestServer(t)
			cookie := signUp(t, srv, "ada@example.com")
			job := createSheet(t, srv, cookie, "Market loft", "10 Market Street")

			res := tryUpload(srv, cookie, job.ID, "big.txt", bytes.Repeat([]byte("a"), size), "text/plain")
			if res.Code != http.StatusBadRequest {
				t.Fatalf("status %d: %s", res.Code, res.Body.String())
			}
			if files := filesUnder(t, dir); len(files) != 0 {
				t.Fatalf("oversized upload left files on disk: %v", files)
			}
			if sheet := getSheet(t, srv, cookie, job.ID); sheet.Filename != "" {
				t.Fatalf("filename %q, want empty", sheet.Filename)
			}
		})
	}

	t.Run("exactly 1 MiB is fine", func(t *testing.T) {
		srv, _ := newTestServer(t)
		cookie := signUp(t, srv, "ada@example.com")
		job := createSheet(t, srv, cookie, "Market loft", "10 Market Street")
		uploadFile(t, srv, cookie, job.ID, "big.txt", bytes.Repeat([]byte("a"), brochure.MaxSize), "text/plain")
	})
}

func TestDownloadReturnsTheBrochureBytes(t *testing.T) {
	srv, _ := newTestServer(t)
	cookie := signUp(t, srv, "ada@example.com")
	job := createSheet(t, srv, cookie, "Market loft", "10 Market Street")

	mustCall(t, srv, http.MethodGet, "/api/jobs/"+job.ID+"/file", cookie, http.StatusNotFound)
	data := []byte("%PDF-1.4\nbinary\x00bytes")
	uploadFile(t, srv, cookie, job.ID, "brochure.pdf", data, "application/pdf")

	res := mustCall(t, srv, http.MethodGet, "/api/jobs/"+job.ID+"/file", cookie, http.StatusOK)
	if !bytes.Equal(res.Body.Bytes(), data) {
		t.Fatalf("download returned %q", res.Body.Bytes())
	}
	if cd := res.Header().Get("Content-Disposition"); !strings.Contains(cd, `"brochure.pdf"`) {
		t.Fatalf("Content-Disposition %q", cd)
	}
}

func TestPageIsRevalidatedWhileAssetsAreCached(t *testing.T) {
	webDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(webDir, "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(webDir, "index.html"), []byte("<html></html>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(webDir, "assets", "index-abc123.js"), []byte("//"), 0o644); err != nil {
		t.Fatal(err)
	}
	srv := New(memstore.New(), t.TempDir(), webDir)

	res := mustCall(t, srv, http.MethodGet, "/", "", http.StatusOK)
	if cc := res.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Fatalf("index Cache-Control %q, want no-cache", cc)
	}
	res = mustCall(t, srv, http.MethodGet, "/assets/index-abc123.js", "", http.StatusOK)
	if cc := res.Header().Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Fatalf("asset Cache-Control %q, want immutable", cc)
	}
}

func TestListHidesOtherUsersJobs(t *testing.T) {
	srv, _ := newTestServer(t)
	ada := signUp(t, srv, "ada@example.com")
	grace := signUp(t, srv, "grace@example.com")
	job := createSheet(t, srv, ada, "Market loft", "10 Market Street")

	res := call(srv, http.MethodGet, "/api/jobs", grace, nil, "")
	if res.Code != http.StatusOK {
		t.Fatalf("status %d: %s", res.Code, res.Body.String())
	}
	var jobs []board.Job
	if err := json.Unmarshal(res.Body.Bytes(), &jobs); err != nil {
		t.Fatal(err)
	}
	for _, item := range jobs {
		if item.ID == job.ID {
			t.Fatal("another account can see this sheet in the list")
		}
	}
}

func TestStartRequiresASession(t *testing.T) {
	srv, _ := newTestServer(t)
	res := call(srv, http.MethodPost, "/api/jobs/"+board.NewID()+"/start", "", nil, "")
	if res.Code != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401", res.Code)
	}
}

func newTestServer(t *testing.T) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	return New(memstore.New(), dir, ""), dir
}

func signUp(t *testing.T, srv *Server, email string) string {
	t.Helper()
	body := strings.NewReader(`{"email":"` + email + `","password":"password123"}`)
	res := call(srv, http.MethodPost, "/api/register", "", body, "application/json")
	if res.Code != http.StatusCreated {
		t.Fatalf("register %d: %s", res.Code, res.Body.String())
	}
	for _, cookie := range res.Result().Cookies() {
		if cookie.Name == sessionCookie {
			return cookie.Value
		}
	}
	t.Fatal("register did not set a session")
	return ""
}

func whoAmI(t *testing.T, srv *Server, cookie string) account {
	t.Helper()
	res := mustCall(t, srv, http.MethodGet, "/api/me", cookie, http.StatusOK)
	var me account
	if err := json.Unmarshal(res.Body.Bytes(), &me); err != nil {
		t.Fatal(err)
	}
	return me
}

func createSheet(t *testing.T, srv *Server, cookie, title, address string) board.Job {
	t.Helper()
	body := strings.NewReader(`{"title":"` + title + `","address":"` + address + `"}`)
	res := call(srv, http.MethodPost, "/api/jobs", cookie, body, "application/json")
	if res.Code != http.StatusCreated {
		t.Fatalf("create %d: %s", res.Code, res.Body.String())
	}
	return decodeJob(t, res)
}

func getSheet(t *testing.T, srv *Server, cookie, id string) board.Job {
	t.Helper()
	return decodeJob(t, mustCall(t, srv, http.MethodGet, "/api/jobs/"+id, cookie, http.StatusOK))
}

// setStatus puts a sheet into a state only the worker would normally reach.
func setStatus(t *testing.T, srv *Server, id, status, message string) {
	t.Helper()
	job, err := srv.store.Job(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	job.Status = status
	job.Error = message
	if err := srv.store.UpdateJob(context.Background(), job); err != nil {
		t.Fatal(err)
	}
}

func uploadFile(t *testing.T, srv *Server, cookie, id, name string, data []byte, contentType string) {
	t.Helper()
	res := tryUpload(srv, cookie, id, name, data, contentType)
	if res.Code != http.StatusOK {
		t.Fatalf("upload %d: %s", res.Code, res.Body.String())
	}
}

func tryUpload(srv *Server, cookie, id, name string, data []byte, contentType string) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	header := textproto.MIMEHeader{}
	// Quote the name the way browsers and multipart.Writer do, so a backslash
	// survives the trip as a backslash.
	quoted := strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(name)
	header.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename="%s"`, quoted))
	header.Set("Content-Type", contentType)
	part, err := writer.CreatePart(header)
	if err != nil {
		panic(err)
	}
	if _, err := part.Write(data); err != nil {
		panic(err)
	}
	if err := writer.Close(); err != nil {
		panic(err)
	}
	return call(srv, http.MethodPost, "/api/jobs/"+id+"/file", cookie, &buf, writer.FormDataContentType())
}

func decodeJob(t *testing.T, res *httptest.ResponseRecorder) board.Job {
	t.Helper()
	var job board.Job
	if err := json.Unmarshal(res.Body.Bytes(), &job); err != nil {
		t.Fatal(err)
	}
	return job
}

func errorMessage(t *testing.T, res *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil {
		t.Fatalf("error body is not JSON: %s", res.Body.String())
	}
	return body.Error
}

// filesUnder lists every regular file below dir, so a test can prove an
// upload wrote exactly one file, or none at all.
func filesUnder(t *testing.T, dir string) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func mustCall(t *testing.T, srv *Server, method, path, cookie string, want int) *httptest.ResponseRecorder {
	t.Helper()
	res := call(srv, method, path, cookie, nil, "")
	if res.Code != want {
		t.Fatalf("%s %s: status %d, want %d: %s", method, path, res.Code, want, res.Body.String())
	}
	return res
}

func call(srv *Server, method, path, cookie string, body io.Reader, contentType string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, body)
	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	res := httptest.NewRecorder()
	srv.Handler().ServeHTTP(res, req)
	return res
}
