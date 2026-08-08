// Package httpapi wires the HTTP surface.
//
// Go 1.22+ gave the stdlib mux method-and-wildcard patterns ("GET /v1/models/{id}"),
// so this repo has ZERO third-party dependencies. Worth saying out loud in an
// interview: reaching for chi/gin/echo is now a choice, not a requirement.
package httpapi

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chasehelton/modelgate/internal/rollout"
	"github.com/chasehelton/modelgate/internal/store"
)

// Metrics is a hand-rolled Prometheus exposition. Deliberately dependency-free
// so you can see exactly what the /metrics text format is instead of it being
// hidden behind a client library.
type Metrics struct {
	Requests     atomic.Int64
	Assignments  sync.Map // model ID -> *atomic.Int64
	ServerErrors atomic.Int64
}

type Server struct {
	store   store.Store
	log     *slog.Logger
	metrics *Metrics
	started time.Time
}

func New(s store.Store, log *slog.Logger) *Server {
	return &Server{store: s, log: log, metrics: &Metrics{}, started: time.Now()}
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /v1/assignment", s.handleAssignment)
	mux.HandleFunc("GET /v1/models", s.handleListModels)
	mux.HandleFunc("PUT /v1/models/{id}/rollout", s.handleSetRollout)
	mux.HandleFunc("POST /v1/models/{id}/disable", s.handleDisable)
	mux.HandleFunc("POST /v1/models/{id}/enable", s.handleEnable)

	// livez: shallow on purpose. It answers "is this process wedged?" only.
	// If you make liveness depend on a database, an outage in that database
	// makes K8s kill every pod you have. That is how a partial outage becomes
	// a total one.
	mux.HandleFunc("GET /livez", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	// readyz: deep. "Should this pod receive traffic right now?" A pod that
	// hasn't loaded its models yet is alive but NOT ready.
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if !s.store.Ready() {
			http.Error(w, "store not loaded", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ready"))
	})

	mux.HandleFunc("GET /metrics", s.handleMetrics)

	return s.withLogging(mux)
}

func (s *Server) handleAssignment(w http.ResponseWriter, r *http.Request) {
	clientID := r.URL.Query().Get("client_id")
	if clientID == "" {
		writeErr(w, http.StatusBadRequest, "client_id query parameter is required")
		return
	}

	a := rollout.Assign(clientID, s.store.List())
	s.countAssignment(a.Model)
	writeJSON(w, http.StatusOK, a)
}

func (s *Server) handleListModels(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"models": s.store.List()})
}

type rolloutRequest struct {
	Percent *int `json:"percent"`
}

func (s *Server) handleSetRollout(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	var req rolloutRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "body must be JSON like {\"percent\": 25}")
		return
	}
	// Pointer, not int: without it we cannot tell {"percent":0} from {}.
	// Distinguishing "set to zero" from "field omitted" matters a lot for a
	// kill-switch-adjacent API.
	if req.Percent == nil {
		writeErr(w, http.StatusBadRequest, "percent field is required")
		return
	}

	err := s.store.SetPercent(id, *req.Percent)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeErr(w, http.StatusNotFound, "no such model: "+id)
		return
	case errors.Is(err, store.ErrBadInput):
		writeErr(w, http.StatusBadRequest, "percent must be between 0 and 100")
		return
	case err != nil:
		s.metrics.ServerErrors.Add(1)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}

	s.log.Info("rollout changed", "model", id, "percent", *req.Percent)
	m, _ := s.store.Get(id)
	writeJSON(w, http.StatusOK, m)
}

func (s *Server) handleDisable(w http.ResponseWriter, r *http.Request) {
	s.setDisabled(w, r, true)
}

func (s *Server) handleEnable(w http.ResponseWriter, r *http.Request) {
	s.setDisabled(w, r, false)
}

func (s *Server) setDisabled(w http.ResponseWriter, r *http.Request, disabled bool) {
	id := r.PathValue("id")
	if err := s.store.SetDisabled(id, disabled); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "no such model: "+id)
			return
		}
		s.metrics.ServerErrors.Add(1)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	s.log.Warn("kill switch toggled", "model", id, "disabled", disabled)
	m, _ := s.store.Get(id)
	writeJSON(w, http.StatusOK, m)
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")

	write := func(s string) { _, _ = w.Write([]byte(s)) }
	write("# HELP modelgate_requests_total Total HTTP requests handled.\n")
	write("# TYPE modelgate_requests_total counter\n")
	write("modelgate_requests_total " + strconv.FormatInt(s.metrics.Requests.Load(), 10) + "\n")

	write("# HELP modelgate_server_errors_total Total 5xx responses.\n")
	write("# TYPE modelgate_server_errors_total counter\n")
	write("modelgate_server_errors_total " + strconv.FormatInt(s.metrics.ServerErrors.Load(), 10) + "\n")

	write("# HELP modelgate_assignments_total Assignments served, by model.\n")
	write("# TYPE modelgate_assignments_total counter\n")
	s.metrics.Assignments.Range(func(k, v any) bool {
		write("modelgate_assignments_total{model=\"" + k.(string) + "\"} " +
			strconv.FormatInt(v.(*atomic.Int64).Load(), 10) + "\n")
		return true
	})

	write("# HELP modelgate_uptime_seconds Seconds since process start.\n")
	write("# TYPE modelgate_uptime_seconds gauge\n")
	write("modelgate_uptime_seconds " + strconv.FormatInt(int64(time.Since(s.started).Seconds()), 10) + "\n")
}

func (s *Server) countAssignment(model string) {
	v, _ := s.metrics.Assignments.LoadOrStore(model, &atomic.Int64{})
	v.(*atomic.Int64).Add(1)
}

// withLogging is a middleware. In Go, middleware is just a function that takes
// an http.Handler and returns one -- no framework required.
func (s *Server) withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.metrics.Requests.Add(1)
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)

		// Health checks fire every few seconds; logging them buries real traffic.
		if r.URL.Path == "/livez" || r.URL.Path == "/readyz" || r.URL.Path == "/healthz" {
			return
		}
		s.log.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"duration_ms", time.Since(start).Milliseconds(),
		)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// TODO(exercise 5): add request latency as a histogram, not just a log line.
// You cannot compute a p99 from a counter -- think about why that matters for
// an SLO, and what bucket boundaries you would pick for this service.
