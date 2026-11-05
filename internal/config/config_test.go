package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDefaultsAreValid(t *testing.T) {
	if err := Default().Validate(); err != nil {
		t.Fatalf("the built-in defaults do not validate: %v", err)
	}
}

// The shipped config.yaml is the first thing a new user edits. If it does not
// load, nothing else matters.
func TestShippedConfigLoads(t *testing.T) {
	cfg, err := Load(filepath.Join("..", "..", "config.yaml"), true)
	if err != nil {
		t.Fatalf("the config.yaml in the repository root does not load: %v", err)
	}
	if cfg.Server.Address == "" {
		t.Error("shipped config has no listen address")
	}
}

func TestMissingFileIsOnlyFatalWhenRequested(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent.yaml")

	if _, err := Load(missing, false); err != nil {
		t.Errorf("a missing default config should fall back to defaults, got: %v", err)
	}
	if _, err := Load(missing, true); err == nil {
		t.Error("a config path given explicitly must exist")
	}
}

func TestDurationsAreParsedAsDurations(t *testing.T) {
	path := writeConfig(t, `
collector:
  interval: 2m
  timeout: 30s
telemetry:
  retention_period: 1h
`)
	cfg, err := Load(path, true)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Collector.Interval.D() != 2*time.Minute {
		t.Errorf("interval = %s, want 2m", cfg.Collector.Interval.D())
	}
	if cfg.Collector.Timeout.D() != 30*time.Second {
		t.Errorf("timeout = %s, want 30s", cfg.Collector.Timeout.D())
	}
}

func TestBadDurationExplainsTheFormat(t *testing.T) {
	path := writeConfig(t, "collector:\n  interval: 30\n")
	_, err := Load(path, true)
	if err == nil {
		t.Fatal("a bare number was accepted as a duration")
	}
	if !strings.Contains(err.Error(), "duration") {
		t.Errorf("error should explain the expected format, got: %v", err)
	}
}

// A silently ignored key leaves an operator convinced they have tuned something
// they have not.
func TestUnknownKeysAreRejected(t *testing.T) {
	path := writeConfig(t, `
recommender:
  thresholds:
    kv_cache_high_pc: 95
`)
	_, err := Load(path, true)
	if err == nil {
		t.Fatal("a mistyped threshold key was silently ignored")
	}
	if !strings.Contains(err.Error(), "kv_cache_high_pc") {
		t.Errorf("error should name the offending key, got: %v", err)
	}
}

func TestValidationRejectsBadConfigurations(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		wantErr string
	}{
		{
			name:    "scrape timeout at or above the interval",
			body:    "collector:\n  interval: 10s\n  timeout: 10s\n",
			wantErr: "shorter than",
		},
		{
			name:    "unknown log level",
			body:    "logging:\n  level: verbose\n",
			wantErr: "logging.level",
		},
		{
			name:    "unknown log format",
			body:    "logging:\n  format: xml\n",
			wantErr: "logging.format",
		},
		{
			name:    "empty listen address",
			body:    "server:\n  address: \"\"\n",
			wantErr: "server.address",
		},
		{
			name: "retention too short for the sustain window",
			body: `
telemetry:
  retention_samples: 10
  retention_period: 10s
recommender:
  thresholds:
    sustain_for: 5m
`,
			wantErr: "retention_period",
		},
		{
			name:    "retention of a single sample",
			body:    "telemetry:\n  retention_samples: 1\n",
			wantErr: "retention_samples",
		},
		{
			name:    "database enabled with no DSN",
			body:    "database:\n  enabled: true\n  dsn: \"\"\n",
			wantErr: "IFA_DATABASE_DSN",
		},
		{
			name:    "alerting enabled with no webhook URL",
			body:    "alerting:\n  enabled: true\n  webhook_url: \"\"\n",
			wantErr: "IFA_ALERTING_WEBHOOK_URL",
		},
		{
			name:    "alerting webhook with a non-http scheme",
			body:    "alerting:\n  enabled: true\n  webhook_url: file:///etc/passwd\n",
			wantErr: "scheme must be http or https",
		},
		{
			name:    "alerting webhook with no host",
			body:    "alerting:\n  enabled: true\n  webhook_url: \"https://\"\n",
			wantErr: "must include a host",
		},
		{
			// Checked even though alerting is off, so a typo is caught at
			// startup rather than on the day someone enables it.
			name:    "unknown min severity while disabled",
			body:    "alerting:\n  min_severity: urgent\n",
			wantErr: "alerting.min_severity",
		},
		{
			name: "alerting timeout at or above the collector interval",
			body: `
collector:
  interval: 5s
  timeout: 2s
alerting:
  enabled: true
  webhook_url: https://hooks.example.com/x
  timeout: 5s
`,
			wantErr: "alerting.timeout",
		},
		{
			name: "alerting queue size of zero",
			body: `
alerting:
  enabled: true
  webhook_url: https://hooks.example.com/x
  queue_size: 0
`,
			wantErr: "alerting.queue_size",
		},
		{
			name: "target with an unsupported runtime",
			body: `
collector:
  targets:
    - name: w
      runtime: tensorrt
      metrics_url: http://x:8000/metrics
`,
			wantErr: "unsupported runtime",
		},
		{
			name: "target with a non-HTTP URL",
			body: `
collector:
  targets:
    - name: w
      runtime: vllm
      metrics_url: file:///etc/passwd
`,
			wantErr: "scheme",
		},
		{
			name: "duplicate targets",
			body: `
collector:
  targets:
    - name: w
      namespace: ns
      runtime: vllm
      metrics_url: http://a:8000/metrics
    - name: w
      namespace: ns
      runtime: vllm
      metrics_url: http://b:8000/metrics
`,
			wantErr: "duplicate",
		},
		{
			name: "GPU bands inverted",
			body: `
recommender:
  thresholds:
    gpu_util_low_pct: 90
    gpu_util_high_pct: 30
`,
			wantErr: "gpu_util_low_pct",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeConfig(t, tc.body), true)
			if err == nil {
				t.Fatalf("expected an error mentioning %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q does not mention %q", err, tc.wantErr)
			}
		})
	}
}

func TestTargetsAreConverted(t *testing.T) {
	path := writeConfig(t, `
collector:
  targets:
    - name: chat
      namespace: inference
      runtime: vLLM
      model_name: meta-llama/Llama-3.1-8B-Instruct
      metrics_url: http://chat.inference.svc:8000/metrics
      dcgm_url: http://dcgm:9400/metrics
      deployment: chat-deploy
`)
	cfg, err := Load(path, true)
	if err != nil {
		t.Fatal(err)
	}
	targets, err := cfg.Targets()
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 1 {
		t.Fatalf("got %d targets", len(targets))
	}
	tgt := targets[0]
	// Runtime names are case-insensitive in config; "vLLM" is what people type.
	if tgt.Runtime != "vllm" {
		t.Errorf("runtime = %q, want vllm", tgt.Runtime)
	}
	if tgt.Deployment != "chat-deploy" || tgt.DCGMURL == "" {
		t.Errorf("target not fully converted: %+v", tgt)
	}
}

func TestAlertingDefaultsAreInertAndUsable(t *testing.T) {
	cfg := Default()
	if cfg.Alerting.Enabled {
		t.Error("alerting must be off by default; posting to an endpoint nobody " +
			"configured is a surprise")
	}
	if cfg.Alerting.MinSeverity != "warning" {
		t.Errorf("default min_severity = %q, want warning", cfg.Alerting.MinSeverity)
	}
	// The defaults must satisfy the alerting.timeout < collector.interval rule,
	// or simply enabling alerting on an otherwise default config fails to start.
	cfg.Alerting.Enabled = true
	cfg.Alerting.WebhookURL = "https://hooks.example.com/services/x"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("enabling alerting on an otherwise default config does not validate: %v", err)
	}
}

func TestAlertingWebhookURLComesFromTheEnvironment(t *testing.T) {
	// Reserved .example host and a hyphenated token: the fixture needs the shape
	// of a real webhook, not a string a secret scanner will mistake for a live
	// credential and refuse to let anyone push.
	const secret = "https://hooks.example.com/services/T0/B0/not-a-real-token-000000"
	t.Setenv(EnvAlertingWebhookURL, secret)

	cfg, err := Load(writeConfig(t, "alerting:\n  enabled: true\n"), true)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Alerting.WebhookURL != secret {
		t.Errorf("webhook URL = %q, want it taken from %s", cfg.Alerting.WebhookURL, EnvAlertingWebhookURL)
	}

	opts := cfg.AlertingOptions()
	if opts.URL != secret {
		t.Errorf("AlertingOptions URL = %q", opts.URL)
	}
	if opts.MinSeverity != "warning" {
		t.Errorf("AlertingOptions MinSeverity = %q, want warning", opts.MinSeverity)
	}
}

// The startup log line is the most likely place for a webhook token to escape,
// because Slack and PagerDuty put the credential in the URL path rather than in
// a userinfo section.
func TestWebhookURLIsRedactedInLogOutput(t *testing.T) {
	const token = "T00000000/B00000000/not-a-real-token-000000"
	cfg := Default()
	cfg.Alerting.Enabled = true
	cfg.Alerting.WebhookURL = "https://hooks.example.com/services/" + token

	out := cfg.String()
	if strings.Contains(out, token) {
		t.Fatalf("config.String() leaked the webhook token: %s", out)
	}
	if !strings.Contains(out, "hooks.example.com") {
		t.Errorf("expected the host to survive redaction: %s", out)
	}
	if !strings.Contains(out, "alerting=true") {
		t.Errorf("expected alerting state in the startup line: %s", out)
	}
}

func TestMinSeverityIsCaseInsensitive(t *testing.T) {
	cfg, err := Load(writeConfig(t, "alerting:\n  min_severity: CRITICAL\n"), true)
	if err != nil {
		t.Fatalf("a capitalised severity should be accepted: %v", err)
	}
	if got := cfg.AlertingOptions().MinSeverity; got != "critical" {
		t.Errorf("MinSeverity = %q, want critical", got)
	}
}

// The DSN carries a password, so it comes from the environment (a Secret in
// Kubernetes) rather than from the ConfigMap the rest of the config lives in.
func TestDatabaseDSNComesFromTheEnvironment(t *testing.T) {
	t.Setenv(EnvDatabaseDSN, "postgres://ifa:hunter2@db:5432/ifa")
	cfg, err := Load(writeConfig(t, "database:\n  enabled: true\n"), true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cfg.Database.DSN, "hunter2") {
		t.Errorf("DSN was not taken from the environment: %q", cfg.Database.DSN)
	}
}

func TestEnvironmentOverrides(t *testing.T) {
	t.Setenv(EnvLogLevel, "debug")
	t.Setenv(EnvAddress, ":9999")
	t.Setenv(EnvClusterName, "prod-eu")

	cfg, err := Load(writeConfig(t, "logging:\n  level: info\n"), true)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Logging.Level != "debug" {
		t.Errorf("log level = %q, want the environment override", cfg.Logging.Level)
	}
	if cfg.Server.Address != ":9999" || cfg.ClusterName != "prod-eu" {
		t.Errorf("overrides not applied: %+v", cfg.Server)
	}
}

// Startup logs routinely end up in a shared log store, so the DSN must not
// appear in them intact.
func TestDSNIsRedactedInLogOutput(t *testing.T) {
	tests := []struct {
		dsn      string
		contains string
		absent   string
	}{
		{dsn: "postgres://ifa:hunter2@db:5432/ifa", contains: "ifa:****@db", absent: "hunter2"},
		{dsn: "postgres://db:5432/ifa", contains: "(set)"},
		{dsn: "not-a-url", contains: "(set)"},
	}
	for _, tc := range tests {
		got := RedactDSN(tc.dsn)
		if !strings.Contains(got, tc.contains) {
			t.Errorf("RedactDSN(%q) = %q, want it to contain %q", tc.dsn, got, tc.contains)
		}
		if tc.absent != "" && strings.Contains(got, tc.absent) {
			t.Errorf("RedactDSN(%q) leaked the password", tc.dsn)
		}
	}

	cfg := Default()
	cfg.Database.Enabled = true
	cfg.Database.DSN = "postgres://ifa:hunter2@db:5432/ifa"
	if strings.Contains(cfg.String(), "hunter2") {
		t.Errorf("the startup log line leaks the password: %s", cfg.String())
	}
}

// ── API authentication ────────────────────────────────────────────────────

func TestAPIAuthDefaultsAreInertAndUsable(t *testing.T) {
	cfg := Default()
	if cfg.APIAuth.Enabled {
		t.Error("api_auth must be off by default to preserve backward compatibility")
	}
	if cfg.APIAuth.Token != "" {
		t.Error("api_auth.token must be empty in the built-in defaults")
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("default config with api_auth disabled does not validate: %v", err)
	}
}

func TestAPIAuthEnabledWithTokenIsValid(t *testing.T) {
	cfg := Default()
	cfg.APIAuth.Enabled = true
	cfg.APIAuth.Token = "a-sufficiently-long-test-token"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("enabled api_auth with a token does not validate: %v", err)
	}
}

func TestAPIAuthEnabledWithoutTokenFails(t *testing.T) {
	cfg := Default()
	cfg.APIAuth.Enabled = true
	cfg.APIAuth.Token = ""
	err := cfg.Validate()
	if err == nil {
		t.Fatal("api_auth.enabled=true with no token should not validate")
	}
	if !strings.Contains(err.Error(), EnvAPIToken) {
		t.Errorf("error should mention %s env var, got: %v", EnvAPIToken, err)
	}
}

func TestAPITokenComesFromEnvironment(t *testing.T) {
	const tok = "env-supplied-test-token-xyz"
	t.Setenv(EnvAPIToken, tok)
	cfg, err := Load(writeConfig(t, "api_auth:\n  enabled: true\n"), true)
	if err != nil {
		t.Fatalf("Load with IFA_API_TOKEN set: %v", err)
	}
	if cfg.APIAuth.Token != tok {
		t.Errorf("Token = %q, want it taken from %s", cfg.APIAuth.Token, EnvAPIToken)
	}
}

func TestAPITokenNotLeakedInLogOutput(t *testing.T) {
	const tok = "super-secret-not-a-real-token-789"
	cfg := Default()
	cfg.APIAuth.Enabled = true
	cfg.APIAuth.Token = tok
	out := cfg.String()
	if strings.Contains(out, tok) {
		t.Fatalf("config.String() leaked the API token: %s", out)
	}
	if !strings.Contains(out, "api_auth=true") {
		t.Errorf("api_auth enabled state missing from startup log: %s", out)
	}
}
