package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/p95labs/ifa/internal/promtext"
)

func render(t *testing.T, r *Registry) *promtext.MetricFamilies {
	t.Helper()
	var b strings.Builder
	r.Write(&b)

	// Parsing our own output with our own parser is the cheapest way to catch
	// an exposition that Prometheus would reject: a metric IFA emits but cannot
	// read is a metric nobody can scrape.
	mf, err := promtext.ParseString(b.String())
	if err != nil {
		t.Fatalf("parsing own exposition: %v\n%s", err, b.String())
	}
	if mf.LinesSkipped != 0 {
		t.Fatalf("own exposition contains %d unparseable line(s):\n%s", mf.LinesSkipped, b.String())
	}
	return mf
}

func TestExpositionIsValidAndTyped(t *testing.T) {
	r := New(BuildInfo{Version: "1.2.3", Commit: "abc123", GoVer: "go1.24"})
	r.RecordScrape("inference/chat", "vllm", 42*time.Millisecond, 0)
	r.RecordScrapeError("inference/broken", "vllm")
	r.RecordAnalysis([]string{"critical", "warning", "warning"})
	r.SetStoreSize(240)

	mf := render(t, r)

	wantTypes := map[string]string{
		"ifa_scrapes_total":                            "counter",
		"ifa_scrape_errors_total":                      "counter",
		"ifa_recommendations_total":                    "counter",
		"ifa_active_recommendations":                   "gauge",
		"ifa_store_snapshots":                          "gauge",
		"ifa_scrape_duration_milliseconds":             "gauge",
		"ifa_last_successful_scrape_timestamp_seconds": "gauge",
		"ifa_build_info":                               "gauge",
	}
	for name, want := range wantTypes {
		got, ok := mf.Type(name)
		if !ok {
			t.Errorf("%s has no TYPE declaration", name)
			continue
		}
		if got != want {
			t.Errorf("%s declared as %s, want %s", name, got, want)
		}
	}

	if v, ok := mf.Sum("ifa_scrapes_total", promtext.Labels{"workload": "inference/chat"}); !ok || v != 1 {
		t.Errorf("scrape counter = %v (ok=%v), want 1", v, ok)
	}
	if v, ok := mf.Sum("ifa_scrape_errors_total", promtext.Labels{"workload": "inference/broken"}); !ok || v != 1 {
		t.Errorf("error counter = %v (ok=%v), want 1", v, ok)
	}
	if v, ok := mf.Sum("ifa_recommendations_total", promtext.Labels{"severity": "warning"}); !ok || v != 2 {
		t.Errorf("warning recommendations = %v, want 2", v)
	}
	if v, ok := mf.Sum("ifa_active_recommendations", nil); !ok || v != 3 {
		t.Errorf("active recommendations = %v, want 3", v)
	}
	if v, ok := mf.Sum("ifa_store_snapshots", nil); !ok || v != 240 {
		t.Errorf("store size = %v, want 240", v)
	}
	if v, ok := mf.Sum("ifa_build_info", promtext.Labels{"version": "1.2.3", "commit": "abc123"}); !ok || v != 1 {
		t.Errorf("build info = %v (ok=%v), want 1 with matching labels", v, ok)
	}
}

// Scrape failures are tracked per target: one unreachable workload out of
// thirty is a very different situation from thirty unreachable ones, and a
// single global counter cannot tell them apart.
func TestScrapeStatsArePerTarget(t *testing.T) {
	r := New(BuildInfo{})
	r.RecordScrape("a", "vllm", time.Millisecond, 0)
	r.RecordScrape("a", "vllm", time.Millisecond, 2)
	r.RecordScrapeError("b", "triton")

	mf := render(t, r)
	if v, _ := mf.Sum("ifa_scrapes_total", promtext.Labels{"workload": "a"}); v != 2 {
		t.Errorf("a scrapes = %v, want 2", v)
	}
	if v, _ := mf.Sum("ifa_scrapes_total", promtext.Labels{"workload": "b"}); v != 0 {
		t.Errorf("b scrapes = %v, want 0", v)
	}
	if v, _ := mf.Sum("ifa_target_missing_metrics", promtext.Labels{"workload": "a"}); v != 2 {
		t.Errorf("a missing metrics = %v, want 2 (from the most recent scrape)", v)
	}
}

// Workload names come from configuration, so a quote or backslash in one would
// otherwise produce output no Prometheus can parse.
func TestLabelValuesAreEscaped(t *testing.T) {
	r := New(BuildInfo{Version: `v1"weird\`})
	r.RecordScrape(`ns/we"ird\name`, "vllm", time.Millisecond, 0)

	mf := render(t, r)
	if _, ok := mf.Sum("ifa_scrapes_total", promtext.Labels{"workload": `ns/we"ird\name`}); !ok {
		t.Errorf("escaped workload label did not round-trip; names: %v", mf.Names())
	}
}

func TestDatabaseMetricsOnlyAppearWhenConfigured(t *testing.T) {
	r := New(BuildInfo{})
	mf := render(t, r)
	if _, ok := mf.Type("ifa_database_writes_total"); ok {
		t.Error("database metrics are exposed without a database configured")
	}

	r.SetDatabaseStats(func() (int64, int64, int64) { return 100, 3, 7 })
	mf = render(t, r)
	if v, ok := mf.Sum("ifa_database_dropped_total", nil); !ok || v != 7 {
		t.Errorf("dropped = %v (ok=%v), want 7", v, ok)
	}
	if v, _ := mf.Sum("ifa_database_write_failures_total", nil); v != 3 {
		t.Errorf("failures = %v, want 3", v)
	}
}

func TestAlertMetricsOnlyAppearWhenConfigured(t *testing.T) {
	r := New(BuildInfo{})
	mf := render(t, r)
	if _, ok := mf.Type("ifa_alerts_sent_total"); ok {
		t.Error("alert metrics are exposed without alerting configured")
	}

	r.SetAlertStats(func() AlertStats {
		return AlertStats{Sent: 12, Failed: 2, Dropped: 5, Suppressed: 900, Open: 3}
	})
	mf = render(t, r)

	for name, want := range map[string]float64{
		"ifa_alerts_sent_total":       12,
		"ifa_alert_failures_total":    2,
		"ifa_alerts_dropped_total":    5,
		"ifa_alerts_suppressed_total": 900,
		"ifa_alerts_open":             3,
	} {
		v, ok := mf.Sum(name, nil)
		if !ok {
			t.Errorf("%s is missing; names: %v", name, mf.Names())
			continue
		}
		if v != want {
			t.Errorf("%s = %v, want %v", name, v, want)
		}
	}

	// Suppressed and dropped are the pair most easily transposed, and they mean
	// opposite things: one is deduplication working, the other is alert loss.
	if typ, _ := mf.Type("ifa_alerts_open"); typ != "gauge" {
		t.Errorf("ifa_alerts_open type = %q, want gauge", typ)
	}
	if typ, _ := mf.Type("ifa_alerts_suppressed_total"); typ != "counter" {
		t.Errorf("ifa_alerts_suppressed_total type = %q, want counter", typ)
	}
}

func TestHandlerRejectsWrites(t *testing.T) {
	r := New(BuildInfo{})
	h := r.Handler()

	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("GET = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("content type = %q", ct)
	}

	rec = httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodPost, "/metrics", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST = %d, want 405", rec.Code)
	}
}

func TestConcurrentUse(t *testing.T) {
	r := New(BuildInfo{})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				r.RecordScrape("w"+string(rune('a'+i)), "vllm", time.Millisecond, j%3)
				r.RecordScrapeError("w"+string(rune('a'+i)), "vllm")
				r.RecordAnalysis([]string{"info"})
				r.SetStoreSize(j)
				var sb strings.Builder
				r.Write(&sb)
			}
		}(i)
	}
	wg.Wait()
}
