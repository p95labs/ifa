package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/p95labs/ifa/internal/telemetry"
)

// Go's flag package stops parsing at the first non-flag argument, so
// `ifa check <url> -runtime vllm` would silently ignore the flag and check the
// wrong runtime — which is worse than an error, because it produces a
// confident, wrong report.
func TestFlagsWorkOnEitherSideOfThePositionalArgument(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		wantPos  string
		wantRest []string
	}{
		{
			name:     "flags after the URL",
			args:     []string{"http://x:8000/metrics", "-runtime", "triton"},
			wantPos:  "http://x:8000/metrics",
			wantRest: []string{"-runtime", "triton"},
		},
		{
			name:     "flags before the URL",
			args:     []string{"-runtime", "triton", "http://x:8000/metrics"},
			wantPos:  "http://x:8000/metrics",
			wantRest: []string{"-runtime", "triton"},
		},
		{
			name:     "equals form does not consume the next argument",
			args:     []string{"-runtime=triton", "http://x:8000/metrics"},
			wantPos:  "http://x:8000/metrics",
			wantRest: []string{"-runtime=triton"},
		},
		{
			name:     "flags on both sides",
			args:     []string{"-model", "m", "http://x/metrics", "-runtime", "vllm"},
			wantPos:  "http://x/metrics",
			wantRest: []string{"-model", "m", "-runtime", "vllm"},
		},
		{
			name:     "no positional at all",
			args:     []string{"-runtime", "vllm"},
			wantPos:  "",
			wantRest: []string{"-runtime", "vllm"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pos, rest := splitPositional(tc.args)
			if pos != tc.wantPos {
				t.Errorf("positional = %q, want %q", pos, tc.wantPos)
			}
			if strings.Join(rest, " ") != strings.Join(tc.wantRest, " ") {
				t.Errorf("rest = %v, want %v", rest, tc.wantRest)
			}
		})
	}
}

func TestUnmeasuredValuesRenderAsADash(t *testing.T) {
	if got := num(telemetry.Metric{}, 1); got != "-" {
		t.Errorf("unmeasured = %q, want %q", got, "-")
	}
	if got := num(telemetry.Observed(0), 1); got != "0.0" {
		t.Errorf("measured zero = %q, want 0.0 — a real zero must not look like missing data", got)
	}
	if got := ms(telemetry.Metric{}); got != "-" {
		t.Errorf("unmeasured latency = %q", got)
	}
	if got := ms(telemetry.Observed(243)); got != "243ms" {
		t.Errorf("sub-second latency = %q, want 243ms", got)
	}
	if got := ms(telemetry.Observed(6913)); got != "6.91s" {
		t.Errorf("multi-second latency = %q, want 6.91s", got)
	}
}

func TestEvidenceFormatting(t *testing.T) {
	got := formatEvidence(telemetry.Evidence{
		Metric: "requests_waiting", Source: "vllm:num_requests_waiting",
		Observed: 29.4, Threshold: 8, Comparison: ">", Unit: "requests",
	})
	for _, want := range []string{"requests_waiting", "29.40", "requests", "> 8", "vllm:num_requests_waiting"} {
		if !strings.Contains(got, want) {
			t.Errorf("formatted evidence %q is missing %q", got, want)
		}
	}

	// Supporting context carries no threshold, so no comparison should appear.
	ctx := formatEvidence(telemetry.Evidence{Metric: "replicas", Observed: 6, Unit: "replicas"})
	if strings.Contains(ctx, ">") || strings.Contains(ctx, "<") {
		t.Errorf("context evidence %q invented a comparison", ctx)
	}
}

func TestCommandsAgainstAStubServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/telemetry":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"items": []telemetry.Snapshot{{
					Timestamp: time.Now().UTC(), Namespace: "inference",
					WorkloadName: "chat", Runtime: telemetry.RuntimeVLLM,
					RequestsWaiting: telemetry.Observed(3),
				}},
				"count": 1,
			})
		case "/api/v1/recommendations":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"items": []telemetry.Recommendation{{
					ID: "IFA-KV-001:inference/chat", Code: "IFA-KV-001",
					Severity: telemetry.SeverityCritical, Namespace: "inference",
					WorkloadName: "chat", Runtime: telemetry.RuntimeVLLM,
					Title: "KV cache exhausted", Explanation: "why", SuggestedAction: "what",
					Evidence: []telemetry.Evidence{{Metric: "preemptions_per_sec", Observed: 1.4}},
				}},
				"count": 1,
			})
		case "/api/v1/rules":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"items": []map[string]any{{
					"code": "IFA-KV-001", "title": "KV cache exhausted",
					"severity": "critical", "summary": "s", "runtimes": []string{"vllm"},
				}},
			})
		case "/api/v1/workloads":
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []any{}, "count": 0})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	tests := []struct {
		name     string
		args     []string
		contains []string
	}{
		{"telemetry", []string{"telemetry", "-url", srv.URL}, []string{"inference/chat", "vllm", "not a zero"}},
		{"telemetry json", []string{"telemetry", "-url", srv.URL, "-json"}, []string{`"items"`}},
		{"recommendations", []string{"recommendations", "-url", srv.URL},
			[]string{"IFA-KV-001", "CRITICAL", "preemptions_per_sec", "why", "what"}},
		{"rules", []string{"rules", "-url", srv.URL}, []string{"IFA-KV-001", "vllm"}},
		{"workloads with none", []string{"workloads", "-url", srv.URL}, []string{"No workloads discovered"}},
		{"version", []string{"version"}, []string{"ifa "}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var out strings.Builder
			if err := run(tc.args, &out); err != nil {
				t.Fatalf("run(%v): %v", tc.args, err)
			}
			for _, want := range tc.contains {
				if !strings.Contains(out.String(), want) {
					t.Errorf("output missing %q:\n%s", want, out.String())
				}
			}
		})
	}
}

func TestServerErrorsSurfaceToTheUser(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"code":"invalid_parameter","message":"severity must be one of ..."}}`))
	}))
	defer srv.Close()

	var out strings.Builder
	err := run([]string{"recommendations", "-url", srv.URL, "-severity", "urgent"}, &out)
	if err == nil {
		t.Fatal("a 400 response was treated as success")
	}
	if !strings.Contains(err.Error(), "400") {
		t.Errorf("error does not mention the status: %v", err)
	}
}

func TestUnknownCommandFails(t *testing.T) {
	var out strings.Builder
	if err := run([]string{"frobnicate"}, &out); err == nil {
		t.Error("an unknown command was accepted")
	}
	if !strings.Contains(out.String(), "Usage:") {
		t.Error("usage was not printed for an unknown command")
	}
}
