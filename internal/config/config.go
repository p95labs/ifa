// Package config loads and validates the control plane's configuration.
//
// Two decisions shape it. Durations are written as durations ("15s", "2m")
// rather than as `*_seconds` integers, because a field named interval_seconds
// holding 300 is read as five minutes by nobody. And unknown keys are an error:
// a mistyped threshold that is silently ignored leaves an operator convinced
// they have tuned something they have not, which is worse than a startup
// failure that names the line.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/p95labs/ifa/internal/alerting"
	"github.com/p95labs/ifa/internal/collector"
	"github.com/p95labs/ifa/internal/recommender"
	"github.com/p95labs/ifa/internal/telemetry"
)

// Duration wraps time.Duration so it can be written in YAML the way people say
// it out loud.
type Duration time.Duration

// UnmarshalYAML parses "15s", "2m30s", "1h".
func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	var raw string
	if err := node.Decode(&raw); err != nil {
		return fmt.Errorf("expected a duration string such as \"15s\": %w", err)
	}
	parsed, err := time.ParseDuration(raw)
	if err != nil {
		return fmt.Errorf("invalid duration %q: use a Go duration such as \"15s\", \"2m\" or \"1h\"", raw)
	}
	*d = Duration(parsed)
	return nil
}

// MarshalYAML renders the duration in the same form it is parsed from.
func (d Duration) MarshalYAML() (any, error) { return time.Duration(d).String(), nil }

// D returns the wrapped time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

// Config is the top-level configuration.
type Config struct {
	ClusterName string            `yaml:"cluster_name"`
	Server      ServerConfig      `yaml:"server"`
	Logging     LoggingConfig     `yaml:"logging"`
	Collector   CollectorConfig   `yaml:"collector"`
	Telemetry   TelemetryConfig   `yaml:"telemetry"`
	Kubernetes  KubernetesConfig  `yaml:"kubernetes"`
	Database    DatabaseConfig    `yaml:"database"`
	Alerting    AlertingConfig    `yaml:"alerting"`
	Recommender RecommenderConfig `yaml:"recommender"`
}

// ServerConfig configures the HTTP listener.
type ServerConfig struct {
	// Address is a Go listen address, e.g. ":8080".
	Address string `yaml:"address"`
	// ReadHeaderTimeout bounds how long a client may take to send request
	// headers. Without it a single idle connection holds a goroutine open
	// indefinitely, which is the cheapest denial of service there is against a
	// Go HTTP server.
	ReadHeaderTimeout Duration `yaml:"read_header_timeout"`
	ReadTimeout       Duration `yaml:"read_timeout"`
	WriteTimeout      Duration `yaml:"write_timeout"`
	IdleTimeout       Duration `yaml:"idle_timeout"`
	ShutdownTimeout   Duration `yaml:"shutdown_timeout"`
}

// LoggingConfig configures the structured logger.
type LoggingConfig struct {
	Level string `yaml:"level"` // debug, info, warn, error
	// Format is "json" or "text". JSON is the default because the primary
	// deployment target is Kubernetes, where logs are collected and parsed.
	Format string `yaml:"format"`
}

// CollectorConfig configures scraping.
type CollectorConfig struct {
	Interval     Duration       `yaml:"interval"`
	Timeout      Duration       `yaml:"timeout"`
	Concurrency  int            `yaml:"concurrency"`
	MaxBodyBytes int64          `yaml:"max_body_bytes"`
	Targets      []TargetConfig `yaml:"targets"`
}

// TargetConfig describes one workload to scrape.
type TargetConfig struct {
	// Name identifies the workload in the API and in recommendations. It does
	// not have to match a Kubernetes object, but joining replica counts
	// requires either a matching Deployment name or an explicit Deployment.
	Name      string `yaml:"name"`
	Namespace string `yaml:"namespace"`
	// Runtime selects the adapter: "vllm" or "triton".
	Runtime string `yaml:"runtime"`
	// ModelName filters metrics when the server hosts several models. Leave
	// empty when it hosts one.
	ModelName  string `yaml:"model_name"`
	MetricsURL string `yaml:"metrics_url"`
	// DCGMURL is an optional DCGM Exporter endpoint. Without it, GPU
	// utilisation and memory are not measured and the GPU rules stay dormant.
	DCGMURL string `yaml:"dcgm_url"`
	// Deployment names the Kubernetes Deployment to join replica counts from.
	// Defaults to Name.
	Deployment string `yaml:"deployment"`
}

// TelemetryConfig bounds in-memory retention.
type TelemetryConfig struct {
	RetentionSamples int      `yaml:"retention_samples"`
	RetentionPeriod  Duration `yaml:"retention_period"`
}

// KubernetesConfig configures workload discovery.
type KubernetesConfig struct {
	Enabled bool `yaml:"enabled"`
	// Namespace to watch. Empty watches all namespaces and requires a
	// ClusterRole; a single namespace only needs a Role.
	Namespace string `yaml:"namespace"`
	// LabelSelector narrows discovery, e.g. "inference.io/runtime".
	LabelSelector string   `yaml:"label_selector"`
	ResyncPeriod  Duration `yaml:"resync_period"`
}

// DatabaseConfig configures the optional durable backend.
type DatabaseConfig struct {
	Enabled bool `yaml:"enabled"`
	// DSN is read from IFA_DATABASE_DSN when that variable is set, so a
	// password never has to live in a ConfigMap.
	DSN       string `yaml:"dsn"`
	QueueSize int    `yaml:"queue_size"`
}

// AlertingConfig configures the optional webhook sender.
//
// It is off by default. IFA's findings are available from the API whether or
// not anything is configured here, and a tool that starts posting to an
// endpoint nobody asked it to post to is a worse surprise than one that stays
// quiet.
type AlertingConfig struct {
	Enabled bool `yaml:"enabled"`
	// WebhookURL is read from IFA_ALERTING_WEBHOOK_URL when that variable is
	// set. Slack, Teams and PagerDuty all embed the credential in the URL
	// itself, so it belongs in a Secret rather than in a ConfigMap — the same
	// reasoning as the database DSN.
	WebhookURL string `yaml:"webhook_url"`
	// MinSeverity is the least severe finding worth sending: info, warning or
	// critical. Findings below it are never sent and never tracked.
	MinSeverity string `yaml:"min_severity"`
	// Timeout bounds a single delivery attempt.
	Timeout Duration `yaml:"timeout"`
	// QueueSize bounds how many alerts may be waiting to be sent before further
	// ones are dropped and counted.
	QueueSize int `yaml:"queue_size"`
}

// RecommenderConfig holds rule thresholds.
type RecommenderConfig struct {
	Thresholds ThresholdConfig `yaml:"thresholds"`
}

// ThresholdConfig mirrors recommender.Thresholds in YAML form.
type ThresholdConfig struct {
	SustainFor           Duration `yaml:"sustain_for"`
	StaleAfter           Duration `yaml:"stale_after"`
	QueueWaitingRequests float64  `yaml:"queue_waiting_requests"`
	GPUUtilHighPct       float64  `yaml:"gpu_util_high_pct"`
	GPUUtilLowPct        float64  `yaml:"gpu_util_low_pct"`
	GPUMemoryHighPct     float64  `yaml:"gpu_memory_high_pct"`
	KVCacheHighPct       float64  `yaml:"kv_cache_high_pct"`
	PreemptionsPerSec    float64  `yaml:"preemptions_per_sec"`
	TTFTP95Ms            float64  `yaml:"ttft_p95_ms"`
	E2EP95Ms             float64  `yaml:"e2e_p95_ms"`
	QueueShareOfTTFTPct  float64  `yaml:"queue_share_of_ttft_pct"`
	TailRatioP99P95      float64  `yaml:"tail_ratio_p99_p95"`
	TokensPerSecondLow   float64  `yaml:"tokens_per_second_low"`
	PrefixCacheHitLowPct float64  `yaml:"prefix_cache_hit_low_pct"`
	ErrorRatePct         float64  `yaml:"error_rate_pct"`
	AbortRatePct         float64  `yaml:"abort_rate_pct"`
}

// Default returns the built-in configuration: a server on :8080 with no scrape
// targets. It is deliberately inert — an IFA that started scraping something it
// was not told about would be a surprise.
func Default() *Config {
	t := recommender.DefaultThresholds()
	return &Config{
		ClusterName: "default",
		Server: ServerConfig{
			Address:           ":8080",
			ReadHeaderTimeout: Duration(5 * time.Second),
			ReadTimeout:       Duration(15 * time.Second),
			WriteTimeout:      Duration(30 * time.Second),
			IdleTimeout:       Duration(60 * time.Second),
			ShutdownTimeout:   Duration(10 * time.Second),
		},
		Logging: LoggingConfig{Level: "info", Format: "json"},
		Collector: CollectorConfig{
			Interval:     Duration(collector.DefaultInterval),
			Timeout:      Duration(collector.DefaultTimeout),
			Concurrency:  collector.DefaultConcurrency,
			MaxBodyBytes: collector.DefaultMaxBodyBytes,
		},
		Telemetry: TelemetryConfig{
			RetentionSamples: 120,
			RetentionPeriod:  Duration(15 * time.Minute),
		},
		Kubernetes: KubernetesConfig{
			Enabled:      false,
			Namespace:    "inference",
			ResyncPeriod: Duration(10 * time.Minute),
		},
		Database: DatabaseConfig{QueueSize: 1024},
		Alerting: AlertingConfig{
			Enabled: false,
			// Warning rather than info: the info-level rules are observations
			// about the telemetry itself, and paging on them would teach the
			// recipient to ignore the channel.
			MinSeverity: string(telemetry.SeverityWarning),
			Timeout:     Duration(alerting.DefaultTimeout),
			QueueSize:   alerting.DefaultQueueSize,
		},
		Recommender: RecommenderConfig{Thresholds: ThresholdConfig{
			SustainFor:           Duration(t.SustainFor),
			StaleAfter:           Duration(t.StaleAfter),
			QueueWaitingRequests: t.QueueWaitingRequests,
			GPUUtilHighPct:       t.GPUUtilHighPct,
			GPUUtilLowPct:        t.GPUUtilLowPct,
			GPUMemoryHighPct:     t.GPUMemoryHighPct,
			KVCacheHighPct:       t.KVCacheHighPct,
			PreemptionsPerSec:    t.PreemptionsPerSec,
			TTFTP95Ms:            t.TTFTP95Ms,
			E2EP95Ms:             t.E2EP95Ms,
			QueueShareOfTTFTPct:  t.QueueShareOfTTFTPct,
			TailRatioP99P95:      t.TailRatioP99P95,
			TokensPerSecondLow:   t.TokensPerSecondLow,
			PrefixCacheHitLowPct: t.PrefixCacheHitLowPct,
			ErrorRatePct:         t.ErrorRatePct,
			AbortRatePct:         t.AbortRatePct,
		}},
	}
}

// Load reads a config file, applies environment overrides and validates the
// result. A missing file at the default path is not an error — the defaults are
// usable — but a path the caller asked for explicitly must exist.
func Load(path string, required bool) (*Config, error) {
	cfg := Default()

	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		dec := yaml.NewDecoder(strings.NewReader(string(data)))
		dec.KnownFields(true)
		if err := dec.Decode(cfg); err != nil {
			return nil, fmt.Errorf("parsing %s: %w", path, err)
		}
	case os.IsNotExist(err) && !required:
		// Defaults it is.
	default:
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}

	applyEnv(cfg)

	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Environment variables recognised as overrides. Only the values that differ
// per deployment are overridable; everything else belongs in the file, where it
// can be reviewed.
const (
	EnvDatabaseDSN        = "IFA_DATABASE_DSN"
	EnvAlertingWebhookURL = "IFA_ALERTING_WEBHOOK_URL"
	EnvLogLevel           = "IFA_LOG_LEVEL"
	EnvLogFormat          = "IFA_LOG_FORMAT"
	EnvAddress            = "IFA_ADDRESS"
	EnvClusterName        = "IFA_CLUSTER_NAME"
)

func applyEnv(cfg *Config) {
	// The DSN carries a password, so it is read from the environment (a Secret
	// in Kubernetes) rather than from the ConfigMap the rest of the config
	// lives in.
	if v := os.Getenv(EnvDatabaseDSN); v != "" {
		cfg.Database.DSN = v
	}
	// The webhook URL is a credential for the same reason, and comes from a
	// Secret in a Helm deployment.
	if v := os.Getenv(EnvAlertingWebhookURL); v != "" {
		cfg.Alerting.WebhookURL = v
	}
	if v := os.Getenv(EnvLogLevel); v != "" {
		cfg.Logging.Level = v
	}
	if v := os.Getenv(EnvLogFormat); v != "" {
		cfg.Logging.Format = v
	}
	if v := os.Getenv(EnvAddress); v != "" {
		cfg.Server.Address = v
	}
	if v := os.Getenv(EnvClusterName); v != "" {
		cfg.ClusterName = v
	}
}

// Validate checks the configuration is internally consistent and usable.
func (c *Config) Validate() error {
	if c.Server.Address == "" {
		return errors.New("config: server.address must not be empty")
	}
	if c.Server.ReadHeaderTimeout <= 0 {
		return errors.New("config: server.read_header_timeout must be positive")
	}
	switch strings.ToLower(c.Logging.Level) {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("config: logging.level must be debug, info, warn or error, got %q", c.Logging.Level)
	}
	switch strings.ToLower(c.Logging.Format) {
	case "json", "text":
	default:
		return fmt.Errorf("config: logging.format must be json or text, got %q", c.Logging.Format)
	}

	if c.Collector.Interval <= 0 {
		return errors.New("config: collector.interval must be positive")
	}
	if c.Collector.Timeout <= 0 {
		return errors.New("config: collector.timeout must be positive")
	}
	if c.Collector.Timeout >= c.Collector.Interval {
		return fmt.Errorf(
			"config: collector.timeout (%s) must be shorter than collector.interval (%s), "+
				"otherwise a slow target keeps scrapes overlapping",
			c.Collector.Timeout.D(), c.Collector.Interval.D())
	}

	names := make(map[string]bool, len(c.Collector.Targets))
	for i, t := range c.Collector.Targets {
		if t.Name == "" {
			return fmt.Errorf("config: collector.targets[%d].name is required", i)
		}
		key := t.Namespace + "/" + t.Name
		if names[key] {
			return fmt.Errorf("config: duplicate target %q", key)
		}
		names[key] = true
		if _, err := t.toCollectorTarget(); err != nil {
			return fmt.Errorf("config: collector.targets[%d]: %w", i, err)
		}
	}

	if c.Telemetry.RetentionSamples <= 1 {
		return errors.New("config: telemetry.retention_samples must be greater than 1; " +
			"rules need more than one sample to evaluate a sustained condition")
	}
	if c.Telemetry.RetentionPeriod <= 0 {
		return errors.New("config: telemetry.retention_period must be positive")
	}

	thresholds := c.Thresholds()
	if err := thresholds.Validate(); err != nil {
		return fmt.Errorf("config: %w", err)
	}
	// Retention has to outlast the window the rules evaluate over, or a
	// sustained condition can never accumulate enough history to fire.
	needed := thresholds.SustainFor * recommender.TrendWindow
	if c.Telemetry.RetentionPeriod.D() < needed {
		return fmt.Errorf(
			"config: telemetry.retention_period (%s) is shorter than %d × recommender sustain_for (%s); "+
				"trend-based rules would never have enough history to fire",
			c.Telemetry.RetentionPeriod.D(), recommender.TrendWindow, needed)
	}

	if c.Database.Enabled && c.Database.DSN == "" {
		return fmt.Errorf("config: database.enabled is true but no DSN is set; "+
			"put it in database.dsn or in the %s environment variable", EnvDatabaseDSN)
	}
	if c.Kubernetes.Enabled && c.Kubernetes.ResyncPeriod <= 0 {
		return errors.New("config: kubernetes.resync_period must be positive")
	}

	if err := c.validateAlerting(); err != nil {
		return err
	}
	return nil
}

func (c *Config) validateAlerting() error {
	// min_severity is checked whether or not alerting is enabled: a typo in a
	// value that only takes effect later is exactly the kind of thing this
	// loader exists to catch at startup.
	switch telemetry.Severity(strings.ToLower(c.Alerting.MinSeverity)) {
	case telemetry.SeverityInfo, telemetry.SeverityWarning, telemetry.SeverityCritical:
	default:
		return fmt.Errorf(
			"config: alerting.min_severity must be info, warning or critical, got %q",
			c.Alerting.MinSeverity)
	}
	if !c.Alerting.Enabled {
		return nil
	}
	if c.Alerting.WebhookURL == "" {
		return fmt.Errorf("config: alerting.enabled is true but no webhook URL is set; "+
			"put it in alerting.webhook_url or in the %s environment variable",
			EnvAlertingWebhookURL)
	}
	if err := alerting.ValidateURL(c.Alerting.WebhookURL); err != nil {
		// The error from ValidateURL does not include the URL, so this cannot
		// print the credential into the startup log.
		return fmt.Errorf("config: %w", err)
	}
	if c.Alerting.Timeout <= 0 {
		return errors.New("config: alerting.timeout must be positive")
	}
	if c.Alerting.Timeout.D() >= c.Collector.Interval.D() {
		// The sender is one goroutine. A timeout longer than the interval means
		// a dead endpoint holds it past the point where the next evaluation
		// produces more alerts, and the queue only ever grows.
		return fmt.Errorf(
			"config: alerting.timeout (%s) must be shorter than collector.interval (%s), "+
				"otherwise a slow endpoint backs the send queue up faster than it drains",
			c.Alerting.Timeout.D(), c.Collector.Interval.D())
	}
	if c.Alerting.QueueSize <= 0 {
		return errors.New("config: alerting.queue_size must be positive")
	}
	return nil
}

// AlertingOptions converts the YAML block into the sender's type. The caller
// supplies the logger.
func (c *Config) AlertingOptions() alerting.Options {
	return alerting.Options{
		URL:         c.Alerting.WebhookURL,
		MinSeverity: telemetry.Severity(strings.ToLower(c.Alerting.MinSeverity)),
		Cluster:     c.ClusterName,
		Timeout:     c.Alerting.Timeout.D(),
		QueueSize:   c.Alerting.QueueSize,
	}
}

// Thresholds converts the YAML threshold block into the recommender's type.
func (c *Config) Thresholds() recommender.Thresholds {
	t := c.Recommender.Thresholds
	return recommender.Thresholds{
		SustainFor:           t.SustainFor.D(),
		StaleAfter:           t.StaleAfter.D(),
		QueueWaitingRequests: t.QueueWaitingRequests,
		GPUUtilHighPct:       t.GPUUtilHighPct,
		GPUUtilLowPct:        t.GPUUtilLowPct,
		GPUMemoryHighPct:     t.GPUMemoryHighPct,
		KVCacheHighPct:       t.KVCacheHighPct,
		PreemptionsPerSec:    t.PreemptionsPerSec,
		TTFTP95Ms:            t.TTFTP95Ms,
		E2EP95Ms:             t.E2EP95Ms,
		QueueShareOfTTFTPct:  t.QueueShareOfTTFTPct,
		TailRatioP99P95:      t.TailRatioP99P95,
		TokensPerSecondLow:   t.TokensPerSecondLow,
		PrefixCacheHitLowPct: t.PrefixCacheHitLowPct,
		ErrorRatePct:         t.ErrorRatePct,
		AbortRatePct:         t.AbortRatePct,
	}
}

// Targets converts the configured targets into collector targets.
func (c *Config) Targets() ([]collector.Target, error) {
	out := make([]collector.Target, 0, len(c.Collector.Targets))
	for i, t := range c.Collector.Targets {
		ct, err := t.toCollectorTarget()
		if err != nil {
			return nil, fmt.Errorf("config: collector.targets[%d]: %w", i, err)
		}
		out = append(out, ct)
	}
	return out, nil
}

func (t TargetConfig) toCollectorTarget() (collector.Target, error) {
	ct := collector.Target{
		WorkloadName: t.Name,
		Namespace:    t.Namespace,
		Runtime:      telemetry.Runtime(strings.ToLower(t.Runtime)),
		ModelName:    t.ModelName,
		MetricsURL:   t.MetricsURL,
		DCGMURL:      t.DCGMURL,
		Deployment:   t.Deployment,
	}
	if err := ct.Validate(); err != nil {
		return collector.Target{}, err
	}
	return ct, nil
}

// String renders the config for the startup log, with the database DSN
// redacted: it routinely contains a password and startup logs routinely end up
// in a shared log store.
func (c *Config) String() string {
	var b strings.Builder
	b.WriteString("cluster=" + c.ClusterName)
	b.WriteString(" address=" + c.Server.Address)
	b.WriteString(" targets=" + strconv.Itoa(len(c.Collector.Targets)))
	b.WriteString(" interval=" + c.Collector.Interval.D().String())
	b.WriteString(" kubernetes=" + strconv.FormatBool(c.Kubernetes.Enabled))
	b.WriteString(" database=" + strconv.FormatBool(c.Database.Enabled))
	if c.Database.Enabled {
		b.WriteString(" dsn=" + RedactDSN(c.Database.DSN))
	}
	b.WriteString(" alerting=" + strconv.FormatBool(c.Alerting.Enabled))
	if c.Alerting.Enabled {
		b.WriteString(" webhook=" + alerting.RedactURL(c.Alerting.WebhookURL))
		b.WriteString(" min_severity=" + strings.ToLower(c.Alerting.MinSeverity))
	}
	return b.String()
}

// RedactDSN removes the password from a PostgreSQL connection string.
func RedactDSN(dsn string) string {
	at := strings.LastIndex(dsn, "@")
	scheme := strings.Index(dsn, "://")
	if at < 0 || scheme < 0 || at < scheme {
		return "(set)"
	}
	creds := dsn[scheme+3 : at]
	if colon := strings.Index(creds, ":"); colon >= 0 {
		return dsn[:scheme+3] + creds[:colon] + ":****" + dsn[at:]
	}
	return dsn
}
