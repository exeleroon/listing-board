package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"listingboard/internal/board"
	"listingboard/internal/brochure"
)

const sessionCookie = "lb_session"

// maxUploadBody caps how much of an upload the API is willing to read before
// it drops the connection. Only the first MaxSize+1 bytes of the file are kept
// in memory; the rest is drained so the 400 reaches the client cleanly instead
// of racing a connection close (port proxies, such as Docker Desktop's, turn
// that race into a reset or a stall).
const maxUploadBody = 64 << 20

// maxFilenameBytes caps the client filename kept on the sheet, like the 200
// character limit on title and address.
const maxFilenameBytes = 255

type Server struct {
	store   board.Store
	dataDir string
	webDir  string
	handler http.Handler
}

type account struct {
	ID    string `json:"id"`
	Email string `json:"email"`
}

func New(store board.Store, dataDir, webDir string) *Server {
	s := &Server{store: store, dataDir: dataDir, webDir: webDir}
	s.handler = withCORS(s.routes())
	return s
}

func (s *Server) Handler() http.Handler {
	return s.handler
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", s.health)
	mux.HandleFunc("POST /api/register", s.register)
	mux.HandleFunc("POST /api/login", s.login)
	mux.HandleFunc("POST /api/logout", s.logout)
	mux.HandleFunc("GET /api/me", s.me)
	mux.HandleFunc("GET /api/jobs", s.listJobs)
	mux.HandleFunc("POST /api/jobs", s.createJob)
	mux.HandleFunc("GET /api/jobs/{id}", s.getJob)
	mux.HandleFunc("POST /api/jobs/{id}/file", s.upload)
	mux.HandleFunc("POST /api/jobs/{id}/start", s.start)
	mux.HandleFunc("GET /api/jobs/{id}/file", s.download)
	if s.webDir != "" {
		mux.Handle("/", webCache(http.FileServer(http.Dir(s.webDir))))
	}
	return mux
}

// webCache keeps index.html fresh and lets the hashed Vite assets be cached
// for good. Without it a browser can hold on to an old index.html whose
// assets no longer exist after a rebuild, and show a blank page.
func webCache(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (s *Server) register(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := decode(w, r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "Request body is not valid JSON")
		return
	}
	email := strings.ToLower(strings.TrimSpace(body.Email))
	if !strings.Contains(email, "@") || len(email) > 200 {
		writeErr(w, http.StatusBadRequest, "Enter an email address")
		return
	}
	if len(body.Password) < 8 || len(body.Password) > 200 {
		writeErr(w, http.StatusBadRequest, "Password must be at least 8 characters")
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(body.Password), bcrypt.DefaultCost)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Could not create the account")
		return
	}
	user, err := s.store.CreateUser(r.Context(), email, string(hash))
	if errors.Is(err, board.ErrDuplicate) {
		writeErr(w, http.StatusConflict, "An account with that email already exists")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Could not create the account")
		return
	}
	if err := s.openSession(w, r, user); err != nil {
		writeErr(w, http.StatusInternalServerError, "Could not start the session")
		return
	}
	writeJSON(w, http.StatusCreated, account{ID: user.ID, Email: user.Email})
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := decode(w, r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "Request body is not valid JSON")
		return
	}
	email := strings.ToLower(strings.TrimSpace(body.Email))
	user, err := s.store.UserByEmail(r.Context(), email)
	if errors.Is(err, board.ErrNotFound) {
		writeErr(w, http.StatusUnauthorized, "That email or password is incorrect")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Could not sign in")
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(body.Password)) != nil {
		writeErr(w, http.StatusUnauthorized, "That email or password is incorrect")
		return
	}
	if err := s.openSession(w, r, user); err != nil {
		writeErr(w, http.StatusInternalServerError, "Could not start the session")
		return
	}
	writeJSON(w, http.StatusOK, account{ID: user.ID, Email: user.Email})
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(sessionCookie); err == nil {
		_ = s.store.DeleteSession(r.Context(), cookie.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode})
	writeJSON(w, http.StatusOK, map[string]any{})
}

func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	user, ok := s.current(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, account{ID: user.ID, Email: user.Email})
}

func (s *Server) listJobs(w http.ResponseWriter, r *http.Request) {
	user, ok := s.current(w, r)
	if !ok {
		return
	}
	jobs, err := s.store.ListJobs(r.Context(), user.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Could not load sheets")
		return
	}
	writeJSON(w, http.StatusOK, jobs)
}

func (s *Server) createJob(w http.ResponseWriter, r *http.Request) {
	user, ok := s.current(w, r)
	if !ok {
		return
	}
	var body struct {
		Title   string `json:"title"`
		Address string `json:"address"`
	}
	if err := decode(w, r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "Request body is not valid JSON")
		return
	}
	title := strings.TrimSpace(body.Title)
	address := strings.TrimSpace(body.Address)
	if title == "" || address == "" || len(title) > 200 || len(address) > 200 {
		writeErr(w, http.StatusBadRequest, "Enter a title and an address")
		return
	}
	now := time.Now().UTC()
	job := board.Job{
		ID:        board.NewID(),
		UserID:    user.ID,
		Title:     title,
		Address:   address,
		Status:    board.StatusDraft,
		Checklist: board.NewChecklist(),
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := s.store.CreateJob(r.Context(), job); err != nil {
		writeErr(w, http.StatusInternalServerError, "Could not create the sheet")
		return
	}
	writeJSON(w, http.StatusCreated, job)
}

func (s *Server) getJob(w http.ResponseWriter, r *http.Request) {
	user, ok := s.current(w, r)
	if !ok {
		return
	}
	job, ok := s.load(w, r, user)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, job)
}

func (s *Server) upload(w http.ResponseWriter, r *http.Request) {
	user, ok := s.current(w, r)
	if !ok {
		return
	}
	job, ok := s.load(w, r, user)
	if !ok {
		return
	}
	if job.Filename != "" {
		writeErr(w, http.StatusBadRequest, "This sheet already has a brochure")
		return
	}
	name, data, ok := readBrochure(w, r)
	if !ok {
		return
	}
	if _, err := brochure.Detect(data); err != nil {
		if errors.Is(err, brochure.ErrTooLarge) {
			writeErr(w, http.StatusBadRequest, "Brochure is too large")
			return
		}
		writeErr(w, http.StatusBadRequest, "Brochure must be a PDF or a text file")
		return
	}
	staged, err := brochure.Stage(s.dataDir, job.UserID, job.ID, data)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Could not store the brochure")
		return
	}
	// Claim the sheet before the bytes go into place: of two uploads racing
	// for the same sheet only one claim succeeds, the other discards its file.
	now := time.Now().UTC()
	claimed, err := s.store.AttachBrochure(r.Context(), job.ID, name, now)
	if errors.Is(err, board.ErrNotFound) {
		discardStaged(staged)
		writeErr(w, http.StatusBadRequest, "This sheet already has a brochure")
		return
	}
	if err != nil {
		discardStaged(staged)
		writeErr(w, http.StatusInternalServerError, "Could not store the brochure")
		return
	}
	if err := brochure.Commit(staged, s.dataDir, job.UserID, job.ID); err != nil {
		log.Printf("upload: place brochure of sheet %s: %v", job.ID, err)
		discardStaged(staged)
		// Give the sheet back, otherwise it would claim a brochure nobody can read.
		claimed.Filename = ""
		claimed.UpdatedAt = now
		if err := s.store.UpdateJob(r.Context(), claimed); err != nil {
			log.Printf("upload: release sheet %s after failed placement: %v", job.ID, err)
		}
		writeErr(w, http.StatusInternalServerError, "Could not store the brochure")
		return
	}
	writeJSON(w, http.StatusOK, claimed)
}

func discardStaged(path string) {
	if err := os.Remove(path); err != nil {
		log.Printf("upload: remove staged brochure %s: %v", path, err)
	}
}

// readBrochure streams the multipart "file" part into memory, keeping at most
// MaxSize+1 bytes so Detect can tell "too large" apart. Nothing touches the
// disk until the bytes have passed the size and type checks, so a rejected
// upload leaves no file behind. It writes the error response itself.
func readBrochure(w http.ResponseWriter, r *http.Request) (string, []byte, bool) {
	if r.ContentLength > maxUploadBody {
		// Far too big to be a brochure; not worth reading at all.
		w.Header().Set("Connection", "close")
		writeErr(w, http.StatusBadRequest, "Brochure is too large")
		return "", nil, false
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBody)
	// Whatever happens, consume the rest of the body before answering.
	defer func() { _, _ = io.Copy(io.Discard, r.Body) }()
	reader, err := r.MultipartReader()
	if err != nil {
		writeErr(w, http.StatusBadRequest, "Brochure upload was incomplete")
		return "", nil, false
	}
	part, err := filePart(reader)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "Choose a brochure file")
		return "", nil, false
	}
	defer part.Close()
	name := filenameFromHeader(part)
	if name == "" {
		writeErr(w, http.StatusBadRequest, "Choose a brochure file")
		return "", nil, false
	}
	if len(name) > maxFilenameBytes {
		writeErr(w, http.StatusBadRequest, "Brochure filename is too long")
		return "", nil, false
	}
	data, err := io.ReadAll(io.LimitReader(part, brochure.MaxSize+1))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "Brochure upload was incomplete")
		return "", nil, false
	}
	return name, data, true
}

// filePart skips other form fields until it reaches the one named "file".
func filePart(reader *multipart.Reader) (*multipart.Part, error) {
	for {
		part, err := reader.NextPart()
		if err != nil {
			return nil, err
		}
		if part.FormName() == "file" {
			return part, nil
		}
		_ = part.Close()
	}
}

func (s *Server) start(w http.ResponseWriter, r *http.Request) {
	user, ok := s.current(w, r)
	if !ok {
		return
	}
	job, ok := s.load(w, r, user)
	if !ok {
		return
	}
	if job.Status == board.StatusDraft {
		if job.Filename == "" {
			writeErr(w, http.StatusBadRequest, "Attach a brochure first")
			return
		}
		started, err := s.store.StartJob(r.Context(), job.ID, board.NewID(), time.Now().UTC())
		switch {
		case errors.Is(err, board.ErrConflict):
			writeErr(w, http.StatusConflict, "Another sheet is already running")
			return
		case errors.Is(err, board.ErrNotFound):
			// A concurrent start won the race; answer with whatever it did.
			if job, ok = s.load(w, r, user); !ok {
				return
			}
		case err != nil:
			writeErr(w, http.StatusInternalServerError, "Could not start the sheet")
			return
		default:
			job = started
		}
	}
	switch job.Status {
	case board.StatusRunning:
		// The worker owns the sheet now; repeat starts just report it with the same runId.
		writeJSON(w, http.StatusOK, job)
	case board.StatusFailed:
		writeErr(w, http.StatusConflict, "This sheet has failed and cannot be started again")
	case board.StatusSuccess:
		writeErr(w, http.StatusConflict, "This sheet has already been published")
	default:
		writeErr(w, http.StatusInternalServerError, "Could not start the sheet")
	}
}

func (s *Server) download(w http.ResponseWriter, r *http.Request) {
	user, ok := s.current(w, r)
	if !ok {
		return
	}
	job, ok := s.load(w, r, user)
	if !ok {
		return
	}
	if job.Filename == "" {
		writeErr(w, http.StatusNotFound, "Not found")
		return
	}
	data, err := brochure.Read(s.dataDir, job.UserID, job.ID)
	if err != nil {
		writeErr(w, http.StatusNotFound, "Not found")
		return
	}
	// The type comes from the bytes, like at upload, not from the client name.
	kind, _ := brochure.Detect(data)
	name := downloadName(job.Filename)
	w.Header().Set("Content-Type", brochure.ContentType(kind))
	w.Header().Set("Content-Disposition", "attachment; filename="+strconvQuote(name))
	http.ServeContent(w, r, name, job.UpdatedAt, bytes.NewReader(data))
}

// load reads the sheet in the path and checks it belongs to the caller. A
// sheet of another account is reported exactly like a missing one.
func (s *Server) load(w http.ResponseWriter, r *http.Request, user board.User) (board.Job, bool) {
	job, err := s.store.Job(r.Context(), r.PathValue("id"))
	if errors.Is(err, board.ErrNotFound) || (err == nil && job.UserID != user.ID) {
		writeErr(w, http.StatusNotFound, "Not found")
		return board.Job{}, false
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Could not load the sheet")
		return board.Job{}, false
	}
	return job, true
}

func (s *Server) current(w http.ResponseWriter, r *http.Request) (board.User, bool) {
	cookie, err := r.Cookie(sessionCookie)
	if err != nil || cookie.Value == "" {
		writeErr(w, http.StatusUnauthorized, "Sign in")
		return board.User{}, false
	}
	user, err := s.store.UserByToken(r.Context(), cookie.Value)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "Sign in")
		return board.User{}, false
	}
	return user, true
}

func (s *Server) openSession(w http.ResponseWriter, r *http.Request, user board.User) error {
	token := board.NewToken()
	if err := s.store.CreateSession(r.Context(), user.ID, token, time.Now().Add(24*time.Hour)); err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   86400,
	})
	return nil
}

// filenameFromHeader returns the client filename as sent. Part.FileName()
// would strip directories, but the spec keeps the original name in the sheet;
// it is never used as a path, see brochure.Path.
func filenameFromHeader(part *multipart.Part) string {
	_, params, err := mime.ParseMediaType(part.Header.Get("Content-Disposition"))
	if err == nil {
		if name := strings.TrimSpace(params["filename"]); name != "" {
			return name
		}
	}
	return strings.TrimSpace(part.FileName())
}

// downloadName is the client filename reduced to its last element, so a
// stored ../../escaped.txt (or ..\..\escaped.txt) is offered as escaped.txt.
func downloadName(filename string) string {
	base := filepath.Base(strings.ReplaceAll(filename, `\`, "/"))
	if base == "." || base == ".." || base == "/" || base == string(filepath.Separator) {
		return "brochure"
	}
	return base
}

func decode(w http.ResponseWriter, r *http.Request, dst any) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	return dec.Decode(dst)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeErr(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func strconvQuote(value string) string {
	b, _ := json.Marshal(value)
	return string(b)
}

func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if origin := r.Header.Get("Origin"); origin == "http://localhost:5173" {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Credentials", "true")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
