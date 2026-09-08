package demo_test

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/p95labs/ifa/internal/collector"
	"github.com/p95labs/ifa/internal/demo"
	"github.com/p95labs/ifa/internal/recommender"
	"github.com/p95labs/ifa/internal/telemetry"
)

// TestDemoScenariosProduceTheirIntendedDiagnosis is the project's end-to-end
// test. It runs the whole pipeline — a real HTTP scrape of vLLM-shaped
// exposition, the real parser, real histogram quantile estimation, real
// counter-to-rate conversion, and the real rule engine — and asserts that each
// simulated failure mode produces the finding it was built to produce.
//
// It is also what keeps the demo honest. The demo is the first thing anyone
// evaluating this project runs; if a scenario silently stops reproducing its
// failure mode, the demo would quietly become a fleet of healthy workloads and
// nobody would notice. Here that fails the build.
func TestDemoScenariosProduceTheirIntendedDiagnosis(t *testing.T) {
	srv, err := demo.NewManualServer(1)
	if err != nil {
		t.Fatalf("starting the simulated fleet: %v", err)
	}
	defer srv.Close()

	const (
		interval   = time.Second
		sustainFor = 15 * time.Second
		steps      = 60
	)

	// Simulated time: the pipeline runs over a minute of workload behaviour in
	// a fraction of a second of real time. The simulated clock is anchored so
	// that the run ends at the present moment, because the store's retention
	// and the staleness rule both measure against the wall clock.
	now := time.Now().UTC().Add(-steps * interval)

	store := telemetry.NewStore(telemetry.WithRetention(200, 10*time.Minute))
	targets := make([]collector.Target, 0, len(srv.Scenarios()))
	for _, sc := range srv.Scenarios() {
		targets = append(targets, collector.Target{
			WorkloadName: sc.Name,
			Namespace:    "inference",
			Runtime:      telemetry.RuntimeVLLM,
			ModelName:    sc.Model,
			MetricsURL:   srv.MetricsURL(sc.Name),
			DCGMURL:      srv.DCGMURL(sc.Name),
		})
	}

	coll, err := collector.New(targets, store, collector.Options{
		Interval:    interval,
		Timeout:     500 * time.Millisecond,
		ClusterName: "demo",
		Logger:      slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError})),
		Workloads:   srv,
		Clock:       func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("building the collector: %v", err)
	}

	for i := 0; i < steps; i++ {
		srv.Advance(interval)
		coll.ScrapeAll(context.Background())
		now = now.Add(interval)
	}

	th := recommender.DefaultThresholds()
	th.SustainFor = sustainFor
	engine := recommender.NewEngine(th)
	recs := engine.Analyze(store.Latest(), store.History)

	byWorkload := map[string]map[string]bool{}
	for _, r := range recs {
		if byWorkload[r.WorkloadName] == nil {
			byWorkload[r.WorkloadName] = map[string]bool{}
		}
		byWorkload[r.WorkloadName][r.Code] = true
	}

	for _, sc := range srv.Scenarios() {
		t.Run(sc.Name, func(t *testing.T) {
			found := byWorkload[sc.Name]
			if sc.Expect == "" {
				// The healthy control. A demo in which every workload is on
				// fire proves only that the thresholds are low.
				if len(found) != 0 {
					t.Errorf("the healthy workload produced findings: %v\n%s",
						keys(found), describe(recs, sc.Name))
				}
				return
			}
			if !found[sc.Expect] {
				t.Errorf("expected %s for %q (%s); got %v",
					sc.Expect, sc.Name, sc.Description, keys(found))
			}
		})
	}

	// Every workload must have been scraped successfully; a demo that silently
	// drops a target would still pass the per-scenario assertions above for the
	// remaining ones.
	if got := len(store.Latest()); got != len(srv.Scenarios()) {
		t.Errorf("collected telemetry for %d of %d workloads", got, len(srv.Scenarios()))
	}
}

// The vLLM adapter must not invent an error rate: vLLM exposes no failure
// counter. This asserts it against a full pipeline run rather than a fixture.
func TestDemoNeverReportsAnErrorRateForVLLM(t *testing.T) {
	srv, err := demo.NewManualServer(7)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	now := time.Now().UTC().Add(-5 * time.Second)
	store := telemetry.NewStore()
	targets := []collector.Target{{
		WorkloadName: srv.Scenarios()[0].Name,
		Namespace:    "inference",
		Runtime:      telemetry.RuntimeVLLM,
		ModelName:    srv.Scenarios()[0].Model,
		MetricsURL:   srv.MetricsURL(srv.Scenarios()[0].Name),
	}}
	coll, err := collector.New(targets, store, collector.Options{
		Interval: time.Second, Timeout: 500 * time.Millisecond,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Clock:  func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 5; i++ {
		srv.Advance(time.Second)
		coll.ScrapeAll(context.Background())
		now = now.Add(time.Second)
	}

	for _, snap := range store.All() {
		if snap.ErrorRatePct.OK {
			t.Fatalf("an error rate of %v was reported for a vLLM workload", snap.ErrorRatePct.Value)
		}
	}
}

func TestScenarioSetIsWellFormed(t *testing.T) {
	scenarios := demo.Scenarios()
	if len(scenarios) < 5 {
		t.Fatalf("only %d scenarios; the demo should cover several distinct failure modes", len(scenarios))
	}

	names := map[string]bool{}
	healthy := 0
	expected := map[string]bool{}
	for _, sc := range scenarios {
		if names[sc.Name] {
			t.Errorf("duplicate scenario name %q", sc.Name)
		}
		names[sc.Name] = true
		if sc.Description == "" {
			t.Errorf("%s has no description", sc.Name)
		}
		if sc.Model == "" {
			t.Errorf("%s has no model name", sc.Name)
		}
		if sc.Expect == "" {
			healthy++
		} else {
			if expected[sc.Expect] {
				t.Errorf("two scenarios both claim to demonstrate %s", sc.Expect)
			}
			expected[sc.Expect] = true
		}
	}
	if healthy != 1 {
		t.Errorf("expected exactly one healthy control scenario, found %d", healthy)
	}
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func describe(recs []telemetry.Recommendation, workload string) string {
	s := ""
	for _, r := range recs {
		if r.WorkloadName == workload {
			s += "  " + r.Code + ": " + r.Explanation + "\n"
		}
	}
	return s
}
