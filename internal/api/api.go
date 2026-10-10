// Package api serves the registry over HTTP and swaps in a fresh database after each sync without downtime.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/jmendozaf/rucpy/internal/ruc"
	"github.com/jmendozaf/rucpy/internal/store"
)

// MaxConcurrentSearches is how many searches run at once; the rest get a 503 right away instead of queueing.
const MaxConcurrentSearches = 2

// cacheFor is how long clients and CDNs may keep a response: the registry changes at most once a day.
const cacheFor = "public, max-age=3600"

// Server answers HTTP requests against the current database.
type Server struct {
	// AllowedOrigins lists the sites whose pages may call the API from the browser (CORS); "*" allows any.
	AllowedOrigins []string

	path string
	// searchSlots caps concurrent searches, the only expensive request, so a burst cannot take every CPU.
	searchSlots chan struct{}
	mu          sync.RWMutex
	st          *store.Store
	log         *slog.Logger
}

// New opens dbPath (if it exists yet) and returns a server for it.
func New(dbPath string, log *slog.Logger) *Server {
	s := &Server{path: dbPath, log: log, searchSlots: make(chan struct{}, MaxConcurrentSearches)}
	if err := s.Reload(); err != nil {
		log.Warn("database not ready yet; run a sync", "path", dbPath, "err", err)
	}
	return s
}

// Reload reopens the database file, e.g. after a sync replaced it.
func (s *Server) Reload() error {
	fresh, err := store.Open(s.path)
	if err != nil {
		return err
	}
	s.mu.Lock()
	old := s.st
	s.st = fresh
	s.mu.Unlock()
	if old != nil {
		old.Close()
	}
	return nil
}

// Close releases the database.
func (s *Server) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.st != nil {
		s.st.Close()
		s.st = nil
	}
}

// Handler returns the HTTP routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /v1/ruc/{ruc}", s.withStore(s.getRUC))
	mux.HandleFunc("GET /v1/search", s.withStore(s.search))
	mux.HandleFunc("GET /v1/changes", s.withStore(s.changes))
	mux.HandleFunc("GET /v1/stats", s.withStore(s.stats))
	return s.cors(mux)
}

// cors lets the allowed sites read the responses. Every route is a simple GET, so there is no preflight to answer.
func (s *Server) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if origin := r.Header.Get("Origin"); origin != "" {
			if slices.Contains(s.AllowedOrigins, "*") {
				w.Header().Set("Access-Control-Allow-Origin", "*")
			} else if slices.Contains(s.AllowedOrigins, origin) {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Add("Vary", "Origin")
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) withStore(h func(http.ResponseWriter, *http.Request, *store.Store)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.mu.RLock()
		defer s.mu.RUnlock()
		if s.st == nil {
			writeError(w, http.StatusServiceUnavailable, "not_synced", "the registry has not been downloaded yet")
			return
		}
		h(w, r, s.st)
	}
}

func (s *Server) getRUC(w http.ResponseWriter, r *http.Request, st *store.Store) {
	input := r.PathValue("ruc")
	parsed, err := ruc.Parse(input)
	if err != nil {
		// Not a RUC; it may still be an old SET code such as "MEFA8203705".
		if t, lookupErr := st.Get(r.Context(), input); lookupErr == nil {
			writeJSON(w, http.StatusOK, t)
			return
		}
		writeError(w, http.StatusBadRequest, "invalid_format", "expected something like 80012345-6")
		return
	}
	if parsed.HasDV && !parsed.Valid() {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error":       "invalid_dv",
			"message":     "the check digit does not match",
			"expected_dv": parsed.ExpectedDV(),
			"formatted":   parsed.Base + "-" + strconv.Itoa(parsed.ExpectedDV()),
		})
		return
	}
	t, err := st.Get(r.Context(), parsed.Base)
	if err == nil || errors.Is(err, store.ErrNotFound) {
		w.Header().Set("Cache-Control", cacheFor)
	}
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "the RUC is not in the DNIT registry")
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, t)
}

func (s *Server) search(w http.ResponseWriter, r *http.Request, st *store.Store) {
	q := r.URL.Query().Get("q")
	if !store.Searchable(q) {
		writeError(w, http.StatusBadRequest, "query_too_short", "q needs a word of at least 3 characters")
		return
	}
	select {
	case s.searchSlots <- struct{}{}:
		defer func() { <-s.searchSlots }()
	default:
		writeError(w, http.StatusServiceUnavailable, "busy", "too many searches right now, try again in a moment")
		return
	}
	results, err := st.Search(r.Context(), q, intParam(r, "limit", 20))
	if err != nil {
		s.fail(w, err)
		return
	}
	w.Header().Set("Cache-Control", cacheFor)
	writeJSON(w, http.StatusOK, map[string]any{"data": results})
}

func (s *Server) changes(w http.ResponseWriter, r *http.Request, st *store.Store) {
	since := time.Now().AddDate(0, 0, -7)
	if raw := r.URL.Query().Get("since"); raw != "" {
		parsed, err := time.Parse(time.DateOnly, raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_since", "since must be YYYY-MM-DD")
			return
		}
		since = parsed
	}
	changes, err := st.Changes(r.Context(), since, r.URL.Query().Get("status"), intParam(r, "limit", 100))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": changes})
}

func (s *Server) stats(w http.ResponseWriter, r *http.Request, st *store.Store) {
	stats, err := st.Stats(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	w.Header().Set("Cache-Control", cacheFor)
	writeJSON(w, http.StatusOK, stats)
}

func (s *Server) fail(w http.ResponseWriter, err error) {
	if errors.Is(err, context.Canceled) {
		return
	}
	s.log.Error("request failed", "err", err)
	writeError(w, http.StatusInternalServerError, "internal", "unexpected error")
}

func intParam(r *http.Request, name string, fallback int) int {
	if n, err := strconv.Atoi(r.URL.Query().Get(name)); err == nil {
		return n
	}
	return fallback
}

func writeError(w http.ResponseWriter, code int, kind, message string) {
	writeJSON(w, code, map[string]string{"error": kind, "message": message})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}
