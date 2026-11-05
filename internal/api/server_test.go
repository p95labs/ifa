package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/p95labs/ifa/internal/recommender"
	"github.com/p95labs/ifa/internal/telemetry"
)

type stubStore struct {
	snaps   []telemetry.Snapshot
	history map[string][]telemetry.Snapshot
}

func (s stubStore) Latest() []telemetry.Snapshot { return s.snaps }
func (s stubStore) History(key string) []telemetry.Snapshot {
	if s.history == nil {
		return nil
	}
	return s.history[key]
}

type stubWorkloads struct{ items []Workload }

func (s stubWorkloads) All() []Workload { return s.items }

type stubMetrics struct{ analyses [][]string }

func (m *stubMetrics) Handler() http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("# HELP ifa_up 1\nifa_up 1\n"))
	}
}
func (m *stubMetrics) RecordAnalysis(sev []string) { m.analyses = append(m.analyses, sev) }

func testServer(t *testing.T, store TelemetrySource, workloads WorkloadSource, m MetricsHandler) (*Server, *http.ServeMux) {
	t.Helper()
	mux := http.NewServeMux()
	srv, err := New(Options{
		Store:     store,
		Workloads: workloads,
		Engine:    recommender.NewEngine(recommender.DefaultThresholds()),
		Metrics:   m,
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Info:      HealthInfo{Version: "test", CollectorMode: "demo"},
	}, mux)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return srv, mux
}

func do(t *testing.T, mux *http.ServeMux, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return rec
}

func snapshot(ns, name string, runtime telemetry.Runtime) telemetry.Snapshot {
	return telemetry.Snapshot{
		Timestamp:       time.Now().UTC(),
		Namespace:       ns,
		WorkloadName:    name,
		Runtime:         runtime,
		RequestsWaiting: telemetry.Observed(1),
	}
}

func TestHealthzAndReadyz(t *testing.T) {
	srv, mux := testServer(t, stubStore{}, nil, nil)

	if got := do(t, mux, http.MethodGet, "/api/v1/healthz").Code; got != http.StatusOK {
		t.Errorf("healthz = %d, want 200", got)
	}

	// Readiness must be distinguishable from liveness or there is no point
	// having both: before the first collection cycle the process is alive but
	// has nothing to serve.
	if got := do(t, mux, http.MethodGet, "/api/v1/readyz").Code; got != http.StatusServiceUnavailable {
		t.Errorf("readyz before the first cycle = %d, want 503", got)
	}
	srv.SetReady()
	if got := do(t, mux, http.MethodGet, "/api/v1/readyz").Code; got != http.StatusOK {
		t.Errorf("readyz after the first cycle = %d, want 200", got)
	}
}

func TestTelemetryEnvelopeAndFilters(t *testing.T) {
	store := stubStore{snaps: []telemetry.Snapshot{
		snapshot("team-a", "one", telemetry.RuntimeVLLM),
		snapshot("team-b", "two", telemetry.RuntimeTriton),
	}}
	_, mux := testServer(t, store, nil, nil)

	var env struct {
		Items []telemetry.Snapshot `json:"items"`
		Count int                  `json:"count"`
	}
	rec := do(t, mux, http.MethodGet, "/api/v1/telemetry")
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decoding envelope: %v", err)
	}
	if env.Count != 2 || len(env.Items) != 2 {
		t.Fatalf("count=%d items=%d, want 2/2", env.Count, len(env.Items))
	}

	rec = do(t, mux, http.MethodGet, "/api/v1/telemetry?namespace=team-a")
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	if env.Count != 1 || env.Items[0].Namespace != "team-a" {
		t.Errorf("namespace filter returned %d items", env.Count)
	}

	rec = do(t, mux, http.MethodGet, "/api/v1/telemetry?runtime=triton")
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	if env.Count != 1 || env.Items[0].Runtime != telemetry.RuntimeTriton {
		t.Errorf("runtime filter returned %d items", env.Count)
	}
}

// The unversioned paths predate /api/v1 and returned bare arrays. Clients built
// against them must keep working.
func TestLegacyPathsReturnBareArrays(t *testing.T) {
	store := stubStore{snaps: []telemetry.Snapshot{snapshot("ns", "one", telemetry.RuntimeVLLM)}}
	_, mux := testServer(t, store, nil, nil)

	for _, path := range []string{"/telemetry", "/recommendations", "/workloads"} {
		rec := do(t, mux, http.MethodGet, path)
		if rec.Code != http.StatusOK {
			t.Errorf("%s = %d", path, rec.Code)
			continue
		}
		var arr []json.RawMessage
		if err := json.Unmarshal(rec.Body.Bytes(), &arr); err != nil {
			t.Errorf("%s did not return a bare array: %v", path, err)
		}
	}
}

// An unmeasured metric has to reach the client as null, not as 0. A dashboard
// that plots a missing GPU reading as zero utilisation is worse than one that
// plots a gap.
func TestUnmeasuredMetricsSerialiseAsNull(t *testing.T) {
	snap := snapshot("ns", "one", telemetry.RuntimeVLLM)
	snap.KVCacheUsagePct = telemetry.Observed(42)
	_, mux := testServer(t, stubStore{snaps: []telemetry.Snapshot{snap}}, nil, nil)

	rec := do(t, mux, http.MethodGet, "/api/v1/telemetry")
	var env struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	item := env.Items[0]
	if item["kv_cache_usage_percent"] != 42.0 {
		t.Errorf("measured value = %v, want 42", item["kv_cache_usage_percent"])
	}
	if v, present := item["gpu_utilization_percent"]; !present || v != nil {
		t.Errorf("unmeasured GPU utilisation = %v, want null", v)
	}
}

func TestInvalidSeverityIsRejected(t *testing.T) {
	_, mux := testServer(t, stubStore{}, nil, nil)

	rec := do(t, mux, http.MethodGet, "/api/v1/recommendations?severity=urgent")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 — a typo must not look like 'no findings'", rec.Code)
	}
	var payload apiError
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("error response is not the standard envelope: %v", err)
	}
	if payload.Error.Code != "invalid_parameter" {
		t.Errorf("error code = %q", payload.Error.Code)
	}
}

func TestWriteMethodsAreRejected(t *testing.T) {
	_, mux := testServer(t, stubStore{}, nil, nil)

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		rec := do(t, mux, method, "/api/v1/telemetry")
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s /api/v1/telemetry = %d, want 405", method, rec.Code)
		}
		if allow := rec.Header().Get("Allow"); allow == "" {
			t.Errorf("%s response has no Allow header", method)
		}
	}
}

func TestUnknownPathReturnsJSONError(t *testing.T) {
	_, mux := testServer(t, stubStore{}, nil, nil)

	rec := do(t, mux, http.MethodGet, "/api/v1/nope")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	var payload apiError
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("404 is not JSON: %v", err)
	}
	if payload.Error.Code != "not_found" {
		t.Errorf("error code = %q", payload.Error.Code)
	}
}

func TestWorkloadsWithoutDiscoveryReturnsEmptyArray(t *testing.T) {
	_, mux := testServer(t, stubStore{}, nil, nil)

	rec := do(t, mux, http.MethodGet, "/workloads")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	// null would force every client to handle a second empty case.
	if body := rec.Body.String(); body != "[]\n" {
		t.Errorf("body = %q, want an empty array", body)
	}
}

func TestWorkloadsAreOrdered(t *testing.T) {
	workloads := stubWorkloads{items: []Workload{
		{Name: "b", Namespace: "ns"},
		{Name: "a", Namespace: "ns"},
		{Name: "a", Namespace: "aa"},
	}}
	_, mux := testServer(t, stubStore{}, workloads, nil)

	rec := do(t, mux, http.MethodGet, "/api/v1/workloads")
	var env struct {
		Items []Workload `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	want := []string{"aa/a", "ns/a", "ns/b"}
	for i, w := range env.Items {
		if got := w.Namespace + "/" + w.Name; got != want[i] {
			t.Errorf("item %d = %s, want %s", i, got, want[i])
		}
	}
}

func TestRulesCatalogueIsServed(t *testing.T) {
	_, mux := testServer(t, stubStore{}, nil, nil)

	rec := do(t, mux, http.MethodGet, "/api/v1/rules")
	var payload struct {
		Items      []RuleDoc      `json:"items"`
		Count      int            `json:"count"`
		Thresholds map[string]any `json:"thresholds"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Count == 0 || len(payload.Items) != payload.Count {
		t.Fatalf("catalogue is empty or inconsistent: count=%d items=%d", payload.Count, len(payload.Items))
	}
	for _, r := range payload.Items {
		if r.Code == "" || r.Title == "" || r.Summary == "" {
			t.Errorf("incomplete rule entry: %+v", r)
		}
	}
	// The thresholds in force belong with the catalogue: a rule code means
	// nothing without the numbers it is comparing against.
	if len(payload.Thresholds) == 0 {
		t.Error("rules response does not include the active thresholds")
	}
	// They have to be readable against the config file, which means the
	// config's own key names and durations written as durations — not Go field
	// names and nanosecond integers.
	if _, ok := payload.Thresholds["kv_cache_high_pct"]; !ok {
		t.Errorf("thresholds do not use configuration key names: %v", payload.Thresholds)
	}
	if got, ok := payload.Thresholds["sustain_for"].(string); !ok {
		t.Errorf("sustain_for = %v, want a duration string", payload.Thresholds["sustain_for"])
	} else if got == "" {
		t.Error("sustain_for is empty")
	}
}

// The self-metric must describe what the engine found, not what one filtered
// request happened to ask for.
func TestAnalysisMetricIsRecordedBeforeFiltering(t *testing.T) {
	now := time.Now().UTC()
	window := make([]telemetry.Snapshot, 0, 20)
	for i := 0; i < 20; i++ {
		s := snapshot("ns", "w", telemetry.RuntimeVLLM)
		s.Timestamp = now.Add(time.Duration(i-19) * 5 * time.Second)
		s.RequestsWaiting = telemetry.Observed(50)
		s.GPUUtilizationPct = telemetry.Observed(97)
		s.KVCacheUsagePct = telemetry.Observed(99)
		window = append(window, s)
	}
	store := stubStore{
		snaps:   []telemetry.Snapshot{window[len(window)-1]},
		history: map[string][]telemetry.Snapshot{"ns/w": window},
	}
	m := &stubMetrics{}
	_, mux := testServer(t, store, nil, m)

	rec := do(t, mux, http.MethodGet, "/api/v1/recommendations?severity=critical")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var env struct {
		Items []telemetry.Recommendation `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	for _, r := range env.Items {
		if r.Severity != telemetry.SeverityCritical {
			t.Errorf("severity filter leaked a %s finding", r.Severity)
		}
	}
	if len(m.analyses) != 1 {
		t.Fatalf("recorded %d analyses, want 1", len(m.analyses))
	}
	if len(m.analyses[0]) <= len(env.Items) {
		t.Errorf("the metric recorded %d findings but the filtered response had %d; "+
			"the metric should describe the full analysis",
			len(m.analyses[0]), len(env.Items))
	}
}

func TestMetricsEndpointIsServed(t *testing.T) {
	_, mux := testServer(t, stubStore{}, nil, &stubMetrics{})
	rec := do(t, mux, http.MethodGet, "/metrics")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if body := rec.Body.String(); body == "" {
		t.Error("metrics endpoint returned nothing")
	}
}

func TestNewRejectsMissingDependencies(t *testing.T) {
	mux := http.NewServeMux()
	if _, err := New(Options{Logger: slog.Default(), Engine: recommender.NewEngine(recommender.DefaultThresholds())}, mux); err == nil {
		t.Error("a server without a store was accepted")
	}
	if _, err := New(Options{Logger: slog.Default(), Store: stubStore{}}, http.NewServeMux()); err == nil {
		t.Error("a server without a rule engine was accepted")
	}
}

// ── Bearer-token authentication ──────────────────────────────────────────────

func testServerWithToken(t *testing.T, token string) *http.ServeMux {
	t.Helper()
	mux := http.NewServeMux()
	_, err := New(Options{
		Store:   stubStore{snaps: []telemetry.Snapshot{snapshot("ns", "w", telemetry.RuntimeVLLM)}},
		Engine:  recommender.NewEngine(recommender.DefaultThresholds()),
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		Metrics: &stubMetrics{},
		Token:   token,
		Info:    HealthInfo{Version: "test"},
	}, mux)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return mux
}

func doWithToken(t *testing.T, mux *http.ServeMux, path, token string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	mux.ServeHTTP(rec, req)
	return rec
}

const testToken = "super-secret-test-token-abc123"

// Auth disabled (Token == ""): all existing behavior is unchanged.
func TestAuth_Disabled_AllDataEndpointsOpen(t *testing.T) {
	mux := testServerWithToken(t, "")
	for _, path := range []string{
		"/api/v1/telemetry",
		"/api/v1/recommendations",
		"/api/v1/workloads",
		"/api/v1/rules",
		"/telemetry",
		"/recommendations",
		"/workloads",
	} {
		rec := do(t, mux, http.MethodGet, path)
		if rec.Code != http.StatusOK {
			t.Errorf("%s with auth disabled = %d, want 200", path, rec.Code)
		}
	}
}

// Health probes must work without credentials regardless of auth setting.
func TestAuth_HealthProbesAlwaysUnauthenticated(t *testing.T) {
	mux := testServerWithToken(t, testToken)
	for _, path := range []string{"/api/v1/healthz", "/api/v1/readyz", "/healthz"} {
		rec := do(t, mux, http.MethodGet, path)
		// readyz returns 503 until SetReady; that is fine — the point is 401
		// is never returned.
		if rec.Code == http.StatusUnauthorized {
			t.Errorf("%s returned 401 — health probes must be unauthenticated", path)
		}
	}
}

// /metrics must be reachable by Prometheus without credentials.
func TestAuth_MetricsAlwaysUnauthenticated(t *testing.T) {
	mux := testServerWithToken(t, testToken)
	rec := do(t, mux, http.MethodGet, "/metrics")
	if rec.Code == http.StatusUnauthorized {
		t.Error("/metrics returned 401 — monitoring scraping must be unauthenticated")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("/metrics = %d, want 200", rec.Code)
	}
}

// Missing Authorization header → 401.
func TestAuth_MissingHeader_Returns401(t *testing.T) {
	mux := testServerWithToken(t, testToken)
	for _, path := range []string{
		"/api/v1/telemetry", "/api/v1/recommendations",
		"/api/v1/workloads", "/api/v1/rules",
		"/telemetry", "/recommendations", "/workloads",
	} {
		rec := do(t, mux, http.MethodGet, path)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s without header = %d, want 401", path, rec.Code)
		}
	}
}

// Malformed header (wrong scheme) → 401.
func TestAuth_MalformedHeader_Returns401(t *testing.T) {
	mux := testServerWithToken(t, testToken)
	for _, badHeader := range []string{
		"Token " + testToken,
		"Basic dXNlcjpwYXNz",
		testToken, // no scheme at all
		"Bearer",  // scheme only, no token
		"",
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/v1/telemetry", nil)
		if badHeader != "" {
			req.Header.Set("Authorization", badHeader)
		}
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("Authorization %q = %d, want 401", badHeader, rec.Code)
		}
	}
}

// Incorrect token value → 401.
func TestAuth_WrongToken_Returns401(t *testing.T) {
	mux := testServerWithToken(t, testToken)
	rec := doWithToken(t, mux, "/api/v1/telemetry", "wrong-token-value")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("wrong token = %d, want 401", rec.Code)
	}
}

// Correct token → 200 for all data endpoints.
func TestAuth_CorrectToken_Returns200(t *testing.T) {
	mux := testServerWithToken(t, testToken)
	for _, path := range []string{
		"/api/v1/telemetry",
		"/api/v1/recommendations",
		"/api/v1/workloads",
		"/api/v1/rules",
		"/telemetry",
		"/recommendations",
		"/workloads",
	} {
		rec := doWithToken(t, mux, path, testToken)
		if rec.Code != http.StatusOK {
			t.Errorf("%s with correct token = %d, want 200", path, rec.Code)
		}
	}
}

// Token must not be leaked in 401 responses.
func TestAuth_TokenNotLeakedInErrorResponse(t *testing.T) {
	mux := testServerWithToken(t, testToken)
	rec := do(t, mux, http.MethodGet, "/api/v1/telemetry")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, testToken) {
		t.Error("the token appears in the 401 response body")
	}
}

// Query-parameter token must be ignored (logs/history leak risk).
func TestAuth_QueryParamToken_Rejected(t *testing.T) {
	mux := testServerWithToken(t, testToken)
	path := "/api/v1/telemetry?token=" + testToken
	rec := do(t, mux, http.MethodGet, path)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("query-param token = %d, want 401 — query strings are unsafe token storage", rec.Code)
	}
}

// 401 response must use the standard error envelope.
func TestAuth_401UsesStandardErrorEnvelope(t *testing.T) {
	mux := testServerWithToken(t, testToken)
	rec := do(t, mux, http.MethodGet, "/api/v1/telemetry")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
	var payload apiError
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("401 body is not the standard error envelope: %v", err)
	}
	if payload.Error.Code != "unauthorized" {
		t.Errorf("error code = %q, want unauthorized", payload.Error.Code)
	}
}

// Legacy endpoints must not bypass authentication.
func TestAuth_LegacyPaths_RequireAuth(t *testing.T) {
	mux := testServerWithToken(t, testToken)
	for _, path := range []string{"/telemetry", "/recommendations", "/workloads"} {
		rec := do(t, mux, http.MethodGet, path)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("legacy %s without auth = %d, want 401", path, rec.Code)
		}
		rec = doWithToken(t, mux, path, testToken)
		if rec.Code != http.StatusOK {
			t.Errorf("legacy %s with correct token = %d, want 200", path, rec.Code)
		}
	}
}

// Write methods on protected paths must still get 405, not 401 — the
// method check runs inside requireAuth so it sees the same token gate.
func TestAuth_WriteMethodsStillReturn405WhenAuthenticated(t *testing.T) {
	mux := testServerWithToken(t, testToken)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/telemetry", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST with valid token = %d, want 405", rec.Code)
	}
}
