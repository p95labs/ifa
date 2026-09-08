// Command ifa is the terminal client for an IFA control
// plane.
//
// It is a thin client on purpose: everything it prints comes from the HTTP API,
// so anything it can show, a dashboard or a script can get too.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/p95labs/ifa/internal/promtext"
	"github.com/p95labs/ifa/internal/runtime"
	"github.com/p95labs/ifa/internal/runtime/triton"
	"github.com/p95labs/ifa/internal/runtime/vllm"
	"github.com/p95labs/ifa/internal/telemetry"
)

const defaultBaseURL = "http://localhost:8080"

var version = "dev"

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "ifa: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string, out io.Writer) error {
	if len(args) == 0 {
		usage(out)
		return errors.New("no command given")
	}

	cmd, args := args[0], args[1:]
	switch cmd {
	case "telemetry":
		return cmdTelemetry(args, out)
	case "recommendations", "recs":
		return cmdRecommendations(args, out)
	case "workloads":
		return cmdWorkloads(args, out)
	case "rules":
		return cmdRules(args, out)
	case "check":
		return cmdCheck(args, out)
	case "version":
		fmt.Fprintf(out, "ifa %s\n", version)
		return nil
	case "help", "-h", "--help":
		usage(out)
		return nil
	default:
		usage(out)
		return fmt.Errorf("unknown command %q", cmd)
	}
}

func usage(out io.Writer) {
	fmt.Fprint(out, `ifa — inspect an IFA control plane

Usage:
  ifa <command> [flags]

Commands:
  telemetry         Latest telemetry snapshot per workload
  recommendations   Active findings, with the evidence behind each one
  workloads         Kubernetes-discovered inference workloads
  rules             The rule catalogue and the thresholds in force
  check <url>       Scrape a runtime's /metrics directly and report which
                    metrics IFA needs are actually present

Common flags:
  -url string       Control plane base URL (default $IFA_URL, else http://localhost:8080)
  -json             Emit raw JSON instead of a table
  -namespace, -workload, -runtime, -severity, -code
                    Server-side filters (where the endpoint supports them)

Examples:
  ifa recommendations -severity critical
  ifa telemetry -namespace inference
  ifa check http://localhost:8000/metrics -runtime vllm
`)
}

// ── Shared flag handling ────────────────────────────────────────────────────

type commonFlags struct {
	baseURL  string
	asJSON   bool
	filters  url.Values
	flagSet  *flag.FlagSet
	timeout  time.Duration
	rawFlags struct {
		namespace, workload, runtime, severity, code string
	}
}

func newFlags(name string) *commonFlags {
	c := &commonFlags{filters: url.Values{}, flagSet: flag.NewFlagSet(name, flag.ContinueOnError)}
	base := os.Getenv("IFA_URL")
	if base == "" {
		base = defaultBaseURL
	}
	c.flagSet.StringVar(&c.baseURL, "url", base, "control plane base URL")
	c.flagSet.BoolVar(&c.asJSON, "json", false, "emit raw JSON")
	c.flagSet.DurationVar(&c.timeout, "timeout", 10*time.Second, "request timeout")
	c.flagSet.StringVar(&c.rawFlags.namespace, "namespace", "", "filter by namespace")
	c.flagSet.StringVar(&c.rawFlags.workload, "workload", "", "filter by workload name")
	c.flagSet.StringVar(&c.rawFlags.runtime, "runtime", "", "filter by runtime")
	c.flagSet.StringVar(&c.rawFlags.severity, "severity", "", "filter by severity: info, warning, critical")
	c.flagSet.StringVar(&c.rawFlags.code, "code", "", "filter by rule code, e.g. IFA-KV-001")
	return c
}

func (c *commonFlags) parse(args []string) error {
	if err := c.flagSet.Parse(args); err != nil {
		return err
	}
	for key, val := range map[string]string{
		"namespace": c.rawFlags.namespace,
		"workload":  c.rawFlags.workload,
		"runtime":   c.rawFlags.runtime,
		"severity":  c.rawFlags.severity,
		"code":      c.rawFlags.code,
	} {
		if val != "" {
			c.filters.Set(key, val)
		}
	}
	return nil
}

func (c *commonFlags) endpoint(path string) string {
	u := strings.TrimSuffix(c.baseURL, "/") + path
	if len(c.filters) > 0 {
		u += "?" + c.filters.Encode()
	}
	return u
}

// ── Commands ────────────────────────────────────────────────────────────────

type envelope[T any] struct {
	Items []T `json:"items"`
	Count int `json:"count"`
}

func cmdTelemetry(args []string, out io.Writer) error {
	f := newFlags("telemetry")
	if err := f.parse(args); err != nil {
		return err
	}

	var env envelope[telemetry.Snapshot]
	raw, err := fetchJSON(f, "/api/v1/telemetry", &env)
	if err != nil {
		return err
	}
	if f.asJSON {
		_, err := out.Write(raw)
		return err
	}
	if len(env.Items) == 0 {
		fmt.Fprintln(out, "No telemetry yet. If the control plane has just started, give it one scrape interval.")
		return nil
	}

	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "WORKLOAD\tRUNTIME\tRUNNING\tWAITING\tKV%\tGPU%\tTTFT-P95\tE2E-P95\tTOK/S\tAGE")
	now := time.Now()
	for _, s := range env.Items {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			s.Key(), s.Runtime,
			num(s.RequestsRunning, 0), num(s.RequestsWaiting, 0),
			num(s.KVCacheUsagePct, 1), num(s.GPUUtilizationPct, 0),
			ms(s.TTFTP95Ms), ms(s.P95LatencyMs),
			num(s.TokensPerSecond, 0),
			shortDuration(now.Sub(s.Timestamp)))
	}
	if err := w.Flush(); err != nil {
		return err
	}
	fmt.Fprintln(out, "\nA dash means the runtime did not report that metric. It is not a zero.")
	return nil
}

func cmdRecommendations(args []string, out io.Writer) error {
	f := newFlags("recommendations")
	if err := f.parse(args); err != nil {
		return err
	}

	var env envelope[telemetry.Recommendation]
	raw, err := fetchJSON(f, "/api/v1/recommendations", &env)
	if err != nil {
		return err
	}
	if f.asJSON {
		_, err := out.Write(raw)
		return err
	}
	if len(env.Items) == 0 {
		fmt.Fprintln(out, "No active findings.")
		return nil
	}

	bySeverity := map[telemetry.Severity]int{}
	for _, r := range env.Items {
		bySeverity[r.Severity]++
	}
	fmt.Fprintf(out, "%d finding(s): %d critical, %d warning, %d info\n\n",
		len(env.Items),
		bySeverity[telemetry.SeverityCritical],
		bySeverity[telemetry.SeverityWarning],
		bySeverity[telemetry.SeverityInfo])

	for _, r := range env.Items {
		fmt.Fprintf(out, "%s  %s  %s\n", severityBadge(r.Severity), r.Code, r.Title)
		fmt.Fprintf(out, "  workload:  %s (%s)\n", workloadKey(r), r.Runtime)
		fmt.Fprintf(out, "  why:       %s\n", wrap(r.Explanation, 76, "             "))
		fmt.Fprintf(out, "  action:    %s\n", wrap(r.SuggestedAction, 76, "             "))
		if len(r.Evidence) > 0 {
			fmt.Fprintln(out, "  evidence:")
			for _, e := range r.Evidence {
				fmt.Fprintf(out, "             %s\n", formatEvidence(e))
			}
		}
		if r.WindowSeconds > 0 {
			fmt.Fprintf(out, "  sustained: %ds\n", r.WindowSeconds)
		}
		fmt.Fprintln(out)
	}
	return nil
}

func cmdWorkloads(args []string, out io.Writer) error {
	f := newFlags("workloads")
	if err := f.parse(args); err != nil {
		return err
	}

	var env envelope[map[string]any]
	raw, err := fetchJSON(f, "/api/v1/workloads", &env)
	if err != nil {
		return err
	}
	if f.asJSON {
		_, err := out.Write(raw)
		return err
	}
	if len(env.Items) == 0 {
		fmt.Fprintln(out, "No workloads discovered. Kubernetes discovery may be disabled (kubernetes.enabled in the config).")
		return nil
	}

	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAMESPACE\tNAME\tRUNTIME\tMODEL\tREPLICAS\tREADY\tGPU\tRESTARTS")
	for _, item := range env.Items {
		fmt.Fprintf(w, "%v\t%v\t%v\t%v\t%v\t%v\t%v\t%v\n",
			field(item, "namespace"), field(item, "name"), field(item, "runtime"),
			field(item, "model_name"), field(item, "replicas"), field(item, "ready_replicas"),
			field(item, "gpu_request"), field(item, "restart_count"))
	}
	return w.Flush()
}

func cmdRules(args []string, out io.Writer) error {
	f := newFlags("rules")
	if err := f.parse(args); err != nil {
		return err
	}

	var payload struct {
		Items []struct {
			Code     string   `json:"code"`
			Title    string   `json:"title"`
			Severity string   `json:"severity"`
			Summary  string   `json:"summary"`
			Runtimes []string `json:"runtimes"`
		} `json:"items"`
	}
	raw, err := fetchJSON(f, "/api/v1/rules", &payload)
	if err != nil {
		return err
	}
	if f.asJSON {
		_, err := out.Write(raw)
		return err
	}

	for _, r := range payload.Items {
		scope := "all runtimes"
		if len(r.Runtimes) > 0 {
			scope = strings.Join(r.Runtimes, ", ")
		}
		fmt.Fprintf(out, "%-12s %-8s %s  [%s]\n", r.Code, r.Severity, r.Title, scope)
		fmt.Fprintf(out, "             %s\n\n", wrap(r.Summary, 76, "             "))
	}
	return nil
}

// cmdCheck scrapes a runtime endpoint directly and reports which of the metrics
// IFA depends on are present.
//
// This exists because the single most common way an integration fails is
// silently: the endpoint responds, the parser finds nothing it recognises, and
// the workload appears healthy because no rule has any input. Making that
// visible before deployment is worth a subcommand.
func cmdCheck(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	runtimeName := fs.String("runtime", "vllm", "runtime adapter to check against: vllm or triton")
	modelName := fs.String("model", "", "model_name to filter on, for servers hosting several models")
	timeout := fs.Duration("timeout", 10*time.Second, "request timeout")

	// The URL is pulled out before flag parsing so that both orderings work.
	// Go's flag package stops at the first non-flag argument, which would make
	// the natural `ifa check <url> -runtime vllm` silently ignore the flag.
	target, rest := splitPositional(args)
	if err := fs.Parse(rest); err != nil {
		return err
	}
	if target == "" || fs.NArg() > 0 {
		return errors.New("usage: ifa check <metrics-url> [-runtime vllm|triton] [-model NAME]")
	}

	var adapter runtime.Adapter
	switch *runtimeName {
	case "vllm":
		adapter = vllm.New()
	case "triton":
		adapter = triton.New()
	default:
		return fmt.Errorf("unknown runtime %q: expected vllm or triton", *runtimeName)
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	body, err := get(ctx, target, *timeout)
	if err != nil {
		return err
	}

	reading, err := adapter.Parse(string(body), *modelName)
	if err != nil {
		return err
	}
	mf, err := promtext.ParseString(string(body))
	if err != nil {
		return err
	}

	fmt.Fprintf(out, "Target:  %s\n", target)
	fmt.Fprintf(out, "Runtime: %s\n", adapter.Runtime())
	if *modelName != "" {
		fmt.Fprintf(out, "Model:   %s\n", *modelName)
	}
	fmt.Fprintf(out, "Payload: %d bytes, %d metric families, %d unparseable line(s)\n\n",
		len(body), len(mf.Names()), reading.UnparseableLines)

	missing := map[string]bool{}
	for _, m := range reading.Missing {
		missing[m] = true
	}
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "STATUS\tMETRIC")
	for _, name := range adapter.ExpectedMetrics() {
		status := "found"
		if missing[name] {
			status = "MISSING"
		} else if len(mf.Select(name, nil)) == 0 && len(mf.Select(name+"_bucket", nil)) == 0 {
			status = "optional, absent"
		}
		fmt.Fprintf(w, "%s\t%s\n", status, name)
	}
	if err := w.Flush(); err != nil {
		return err
	}

	if len(reading.Missing) == 0 {
		fmt.Fprintln(out, "\nAll required metrics present.")
		return nil
	}

	fmt.Fprintf(out, "\n%d required metric(s) missing. Rules that depend on them will not run.\n",
		len(reading.Missing))
	if adapter.Runtime() == telemetry.RuntimeVLLM {
		fmt.Fprintln(out, "For vLLM: confirm the server was not started with --disable-log-stats,")
		fmt.Fprintln(out, "and that -model matches the model_name label on the series.")
		if *modelName != "" {
			if models := vllmModels(mf); len(models) > 0 {
				fmt.Fprintf(out, "Models present in this payload: %s\n", strings.Join(models, ", "))
			}
		}
	}
	if adapter.Runtime() == telemetry.RuntimeTriton {
		if models, err := triton.Models(string(body)); err == nil && len(models) > 0 {
			fmt.Fprintf(out, "Models present in this payload: %s\n", strings.Join(models, ", "))
		}
	}
	return nil
}

// splitPositional returns the first bare argument and everything else, so a
// positional argument may appear before or after the flags.
func splitPositional(args []string) (positional string, rest []string) {
	skipValue := false
	for i, a := range args {
		if skipValue {
			skipValue = false
			rest = append(rest, a)
			continue
		}
		if strings.HasPrefix(a, "-") {
			rest = append(rest, a)
			// A flag written as "-model NAME" takes the next argument as its
			// value; "-model=NAME" does not.
			if !strings.Contains(a, "=") {
				skipValue = true
			}
			continue
		}
		positional = a
		rest = append(rest, args[i+1:]...)
		return positional, rest
	}
	return "", rest
}

func vllmModels(mf *promtext.MetricFamilies) []string {
	set := map[string]bool{}
	for _, name := range []string{vllm.MetricNumRequestsRunning, vllm.MetricNumRequestsWaiting} {
		for _, s := range mf.Select(name, nil) {
			if m := s.Labels[vllm.LabelModelName]; m != "" {
				set[m] = true
			}
		}
	}
	out := make([]string, 0, len(set))
	for m := range set {
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}

// ── HTTP and formatting helpers ─────────────────────────────────────────────

func fetchJSON(f *commonFlags, path string, into any) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), f.timeout)
	defer cancel()

	body, err := get(ctx, f.endpoint(path), f.timeout)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(body, into); err != nil {
		return nil, fmt.Errorf("decoding response from %s: %w", path, err)
	}
	return body, nil
}

func get(ctx context.Context, target string, timeout time.Duration) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, fmt.Errorf("building request for %s: %w", target, err)
	}
	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("requesting %s: %w", target, err)
	}
	defer resp.Body.Close()

	// 16 MiB is far beyond any legitimate response and keeps a runaway server
	// from exhausting the client.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, fmt.Errorf("reading response from %s: %w", target, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s returned %s: %s", target, resp.Status, firstLine(body))
	}
	return body, nil
}

func firstLine(b []byte) string {
	s := strings.TrimSpace(string(b))
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

// num renders a Metric, or "-" when it was not measured. The distinction is the
// point: a dash means the runtime never reported the value.
func num(m telemetry.Metric, decimals int) string {
	if !m.OK {
		return "-"
	}
	return fmt.Sprintf("%.*f", decimals, m.Value)
}

func ms(m telemetry.Metric) string {
	if !m.OK {
		return "-"
	}
	if m.Value >= 1000 {
		return fmt.Sprintf("%.2fs", m.Value/1000)
	}
	return fmt.Sprintf("%.0fms", m.Value)
}

func shortDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
}

func severityBadge(s telemetry.Severity) string {
	switch s {
	case telemetry.SeverityCritical:
		return "[CRITICAL]"
	case telemetry.SeverityWarning:
		return "[WARNING] "
	default:
		return "[INFO]    "
	}
}

func workloadKey(r telemetry.Recommendation) string {
	if r.Namespace == "" {
		return r.WorkloadName
	}
	return r.Namespace + "/" + r.WorkloadName
}

func formatEvidence(e telemetry.Evidence) string {
	var b strings.Builder
	b.WriteString(e.Metric)
	b.WriteString(" = ")
	b.WriteString(trimFloat(e.Observed))
	if e.Unit != "" {
		b.WriteString(" " + e.Unit)
	}
	if e.Comparison != "" {
		b.WriteString(fmt.Sprintf(" (%s %s)", e.Comparison, trimFloat(e.Threshold)))
	}
	if e.Source != "" {
		b.WriteString("  ← " + e.Source)
	}
	return b.String()
}

func trimFloat(v float64) string {
	s := fmt.Sprintf("%.2f", v)
	s = strings.TrimSuffix(s, ".00")
	return s
}

// wrap breaks text at width, indenting continuation lines.
func wrap(text string, width int, indent string) string {
	words := strings.Fields(text)
	if len(words) == 0 {
		return ""
	}
	var b strings.Builder
	line := 0
	for i, word := range words {
		if i > 0 && line+len(word)+1 > width {
			b.WriteString("\n" + indent)
			line = 0
		} else if i > 0 {
			b.WriteString(" ")
			line++
		}
		b.WriteString(word)
		line += len(word)
	}
	return b.String()
}

func field(m map[string]any, key string) any {
	if v, ok := m[key]; ok && v != nil {
		if f, isFloat := v.(float64); isFloat {
			return int64(f)
		}
		return v
	}
	return "-"
}
