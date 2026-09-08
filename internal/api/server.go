// Package api serves IFA's read-only HTTP interface.
//
// Every handler is a GET. There is no endpoint that changes anything, in IFA or
// in the cluster, and there is deliberately no way to add one without changing
// this package: the read-only guarantee in the README has to be checkable by
// reading one file.
package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/p95labs/ifa/internal/recommender"
	"github.com/p95labs/ifa/internal/telemetry"
)

// TelemetrySource is the read side of the telemetry store.
type TelemetrySource interface {
	Latest() []telemetry.Snapshot
	History(key string) []telemetry.Snapshot
}

// WorkloadSource supplies Kubernetes-discovered workloads.
type WorkloadSource interface {
	All() []Workload
}

// Workload is the API representation of a discovered Kubernetes workload.
// It is defined here rather than reused from the watcher so that the wire
// format does not change whenever the watcher's internal struct does.
type Workload struct {
	Name          string            `json:"name"`
	Namespace     string            `json:"namespace"`
	Runtime       string            `json:"runtime,omitempty"`
	ModelName     string            `json:"model_name,omitempty"`
	Replicas      int32             `json:"replicas"`
	ReadyReplicas int32             `json:"ready_replicas"`
	RestartCount  int32             `json:"restart_count"`
	GPURequest    string            `json:"gpu_request,omitempty"`
	Labels        map[string]string `json:"labels,omitempty"`
	LastUpdated   time.Time         `json:"last_updated"`
}

// MetricsHandler renders IFA's own Prometheus metrics.
type MetricsHandler interface {
	Handler() http.HandlerFunc
	RecordAnalysis(severities []string)
}

// Options configures the server.
type Options struct {
	Store     TelemetrySource
	Workloads WorkloadSource
	Engine    *recommender.Engine
	Metrics   MetricsHandler
	Logger    *slog.Logger
	// Info is reported by /api/v1/healthz so an operator can confirm which
	// configuration a running instance actually loaded.
	Info HealthInfo
}

// HealthInfo describes the running configuration.
type HealthInfo struct {
	Version        string `json:"version"`
	Commit         string `json:"commit,omitempty"`
	CollectorMode  string `json:"collector_mode"`
	TargetCount    int    `json:"target_count"`
	KubernetesMode string `json:"kubernetes"`
	DatabaseMode   string `json:"database"`
}

// Server holds handler dependencies.
type Server struct {
	opts    Options
	started time.Time
	// ready flips once the first collection cycle has completed. Liveness says
	// the process is running; readiness says it has finished starting up and
	// has had a chance to populate the store. Making readiness depend on
	// scrapes *succeeding* would be wrong: an instance that cannot reach its
	// targets is exactly the instance an operator needs to be able to query.
	ready atomic.Bool
}

// New wires the routes onto mux and returns the server.
func New(opts Options, mux *http.ServeMux) (*Server, error) {
	if opts.Store == nil {
		return nil, errors.New("api: store is required")
	}
	if opts.Engine == nil {
		return nil, errors.New("api: recommender engine is required")
	}
	if opts.Logger == nil {
		return nil, errors.New("api: logger is required")
	}

	s := &Server{opts: opts, started: time.Now()}

	routes := map[string]http.HandlerFunc{
		"/api/v1/healthz":         s.handleHealthz,
		"/api/v1/readyz":          s.handleReadyz,
		"/api/v1/telemetry":       s.handleTelemetry,
		"/api/v1/recommendations": s.handleRecommendations,
		"/api/v1/workloads":       s.handleWorkloads,
		"/api/v1/rules":           s.handleRules,
	}
	for path, h := range routes {
		mux.Handle(path, getOnly(h))
	}

	// Unversioned paths from before /api/v1 existed. They return the bare
	// arrays the original API returned rather than the versioned envelope, so
	// existing clients keep working unchanged. See docs/API.md.
	legacy := map[string]http.HandlerFunc{
		"/healthz":         s.handleHealthz,
		"/telemetry":       s.handleLegacyTelemetry,
		"/recommendations": s.handleLegacyRecommendations,
		"/workloads":       s.handleLegacyWorkloads,
	}
	for path, h := range legacy {
		mux.Handle(path, getOnly(h))
	}

	if opts.Metrics != nil {
		mux.Handle("/metrics", opts.Metrics.Handler())
	}

	// ServeMux's "/" pattern matches everything unmatched, which is how an
	// unknown path gets a JSON 404 instead of net/http's plain-text one.
	mux.HandleFunc("/", s.handleNotFound)

	return s, nil
}

// SetReady marks the server ready. The collector calls it after its first
// cycle.
func (s *Server) SetReady() { s.ready.Store(true) }

// ── Handlers ────────────────────────────────────────────────────────────────

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":         "ok",
		"uptime_seconds": int(time.Since(s.started).Seconds()),
		"config":         s.opts.Info,
	})
}

func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	if !s.ready.Load() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"status": "starting",
			"reason": "the first collection cycle has not completed",
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ready"})
}

func (s *Server) handleTelemetry(w http.ResponseWriter, r *http.Request) {
	snaps, err := s.filteredTelemetry(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_parameter", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, envelope[telemetry.Snapshot]{
		Items:       snaps,
		Count:       len(snaps),
		GeneratedAt: time.Now().UTC(),
	})
}

func (s *Server) handleLegacyTelemetry(w http.ResponseWriter, r *http.Request) {
	snaps, err := s.filteredTelemetry(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_parameter", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, snaps)
}

func (s *Server) handleRecommendations(w http.ResponseWriter, r *http.Request) {
	recs, err := s.filteredRecommendations(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_parameter", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, envelope[telemetry.Recommendation]{
		Items:       recs,
		Count:       len(recs),
		GeneratedAt: time.Now().UTC(),
	})
}

func (s *Server) handleLegacyRecommendations(w http.ResponseWriter, r *http.Request) {
	recs, err := s.filteredRecommendations(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_parameter", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, recs)
}

func (s *Server) handleWorkloads(w http.ResponseWriter, r *http.Request) {
	items := s.workloads()
	writeJSON(w, http.StatusOK, envelope[Workload]{
		Items:       items,
		Count:       len(items),
		GeneratedAt: time.Now().UTC(),
	})
}

func (s *Server) handleLegacyWorkloads(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.workloads())
}

// RuleDoc is the API representation of a rule. Serving the catalogue makes the
// rule set inspectable without reading the source, and lets the docs be
// generated from the same data the engine runs.
type RuleDoc struct {
	Code       string   `json:"code"`
	Title      string   `json:"title"`
	Severity   string   `json:"severity"`
	Summary    string   `json:"summary"`
	Runtimes   []string `json:"runtimes,omitempty"`
	Supersedes []string `json:"supersedes,omitempty"`
}

func (s *Server) handleRules(w http.ResponseWriter, r *http.Request) {
	rules := s.opts.Engine.Rules()
	items := make([]RuleDoc, 0, len(rules))
	for _, rule := range rules {
		doc := RuleDoc{
			Code:     string(rule.Code),
			Title:    rule.Title,
			Severity: string(rule.Severity),
			Summary:  rule.Summary,
		}
		for _, rt := range rule.Runtimes {
			doc.Runtimes = append(doc.Runtimes, string(rt))
		}
		for _, c := range rule.Supersedes {
			doc.Supersedes = append(doc.Supersedes, string(c))
		}
		items = append(items, doc)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items":      items,
		"count":      len(items),
		"thresholds": s.opts.Engine.Thresholds(),
	})
}

func (s *Server) handleNotFound(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotFound, "not_found",
		"unknown path; see /api/v1/healthz, /api/v1/telemetry, /api/v1/recommendations, "+
			"/api/v1/workloads, /api/v1/rules, /metrics")
}

// ── Shared logic ────────────────────────────────────────────────────────────

type envelope[T any] struct {
	Items       []T       `json:"items"`
	Count       int       `json:"count"`
	GeneratedAt time.Time `json:"generated_at"`
}

func (s *Server) filteredTelemetry(r *http.Request) ([]telemetry.Snapshot, error) {
	f, err := parseFilter(r)
	if err != nil {
		return nil, err
	}
	all := s.opts.Store.Latest()
	out := make([]telemetry.Snapshot, 0, len(all))
	for _, snap := range all {
		if f.matchesSnapshot(snap) {
			out = append(out, snap)
		}
	}
	return out, nil
}

func (s *Server) filteredRecommendations(r *http.Request) ([]telemetry.Recommendation, error) {
	f, err := parseFilter(r)
	if err != nil {
		return nil, err
	}

	recs := s.opts.Engine.Analyze(s.opts.Store.Latest(), s.opts.Store.History)

	// Record the full analysis before filtering: the self-metric should
	// describe what the engine found, not what one client asked to see.
	if s.opts.Metrics != nil {
		severities := make([]string, 0, len(recs))
		for _, rec := range recs {
			severities = append(severities, string(rec.Severity))
		}
		s.opts.Metrics.RecordAnalysis(severities)
	}

	out := make([]telemetry.Recommendation, 0, len(recs))
	for _, rec := range recs {
		if f.matchesRecommendation(rec) {
			out = append(out, rec)
		}
	}
	return out, nil
}

func (s *Server) workloads() []Workload {
	if s.opts.Workloads == nil {
		return []Workload{}
	}
	items := s.opts.Workloads.All()
	if items == nil {
		return []Workload{}
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Namespace != items[j].Namespace {
			return items[i].Namespace < items[j].Namespace
		}
		return items[i].Name < items[j].Name
	})
	return items
}

// filter holds the query parameters shared by the list endpoints.
type filter struct {
	namespace string
	workload  string
	runtime   string
	severity  telemetry.Severity
	code      string
}

var validSeverities = map[telemetry.Severity]bool{
	telemetry.SeverityInfo:     true,
	telemetry.SeverityWarning:  true,
	telemetry.SeverityCritical: true,
}

func parseFilter(r *http.Request) (filter, error) {
	q := r.URL.Query()
	f := filter{
		namespace: q.Get("namespace"),
		workload:  q.Get("workload"),
		runtime:   q.Get("runtime"),
		code:      strings.ToUpper(q.Get("code")),
	}
	if raw := q.Get("severity"); raw != "" {
		sev := telemetry.Severity(strings.ToLower(raw))
		if !validSeverities[sev] {
			// Rejecting an unknown value rather than returning everything
			// keeps a typo from looking like "no findings at this severity".
			return filter{}, errors.New(`severity must be one of "info", "warning", "critical"`)
		}
		f.severity = sev
	}
	return f, nil
}

func (f filter) matchesSnapshot(s telemetry.Snapshot) bool {
	switch {
	case f.namespace != "" && s.Namespace != f.namespace:
		return false
	case f.workload != "" && s.WorkloadName != f.workload:
		return false
	case f.runtime != "" && string(s.Runtime) != f.runtime:
		return false
	}
	return true
}

func (f filter) matchesRecommendation(r telemetry.Recommendation) bool {
	switch {
	case f.namespace != "" && r.Namespace != f.namespace:
		return false
	case f.workload != "" && r.WorkloadName != f.workload:
		return false
	case f.runtime != "" && string(r.Runtime) != f.runtime:
		return false
	case f.severity != "" && r.Severity != f.severity:
		return false
	case f.code != "" && r.Code != f.code:
		return false
	}
	return true
}

// getOnly rejects anything but GET and HEAD. The API is read-only by design;
// returning 405 rather than quietly treating a POST as a GET makes that
// explicit to anyone probing the surface.
func getOnly(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed",
				"this API is read-only; only GET and HEAD are accepted")
			return
		}
		h(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	// An encode failure here means the response is already partially written,
	// so the status line cannot be corrected. Nothing useful can be done
	// beyond letting the client see a truncated body; the caller logs.
	_ = enc.Encode(payload)
}

// apiError is the single error shape every endpoint returns.
type apiError struct {
	Error errorBody `json:"error"`
}

type errorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, apiError{Error: errorBody{Code: code, Message: message}})
}
