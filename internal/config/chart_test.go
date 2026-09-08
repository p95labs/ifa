package config

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

// The Helm chart carries its own copy of the configuration under values.config,
// which is rendered verbatim into a ConfigMap. Because the loader rejects
// unknown keys, a field renamed in Go and not in the chart turns into a pod
// that crash-loops on startup — and the chart is exactly the path where nobody
// notices until it is deployed.
//
// This test loads the chart's block through the real loader, so the two cannot
// drift apart without the build failing.
func TestChartValuesMatchTheConfigSchema(t *testing.T) {
	valuesPath := filepath.Join("..", "..", "deploy", "helm", "ifa", "values.yaml")
	raw, err := os.ReadFile(valuesPath)
	if err != nil {
		t.Fatalf("reading chart values: %v", err)
	}

	var values struct {
		Config yaml.Node `yaml:"config"`
	}
	if err := yaml.Unmarshal(raw, &values); err != nil {
		t.Fatalf("parsing chart values: %v", err)
	}
	if values.Config.IsZero() {
		t.Fatal("the chart has no config block")
	}

	rendered, err := yaml.Marshal(values.Config)
	if err != nil {
		t.Fatalf("re-rendering the chart's config block: %v", err)
	}

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, rendered, 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path, true)
	if err != nil {
		t.Fatalf("the chart's default config does not load:\n%v\n\nrendered config:\n%s", err, rendered)
	}
	if cfg.Server.Address == "" {
		t.Error("the chart's config has no listen address")
	}
	// The chart enables Kubernetes discovery by default, which is the whole
	// reason to run IFA in a cluster rather than locally.
	if !cfg.Kubernetes.Enabled {
		t.Error("the chart's default config has Kubernetes discovery disabled")
	}
}
