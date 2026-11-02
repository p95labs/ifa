#!/usr/bin/env bash
# scripts/alerting-integration.sh
#
# Run the webhook alerting integration test suite.
#
# This script runs the Go tests tagged //go:build integration from
# internal/alerting/. Those tests start a real IFA pipeline in-process, serve
# a controllable vLLM-format Prometheus fixture over a local HTTP server, and
# assert on webhook payloads delivered to an httptest.Server receiver. No
# external services, Docker containers, or cloud credentials are required.
#
# It also renders the Helm chart with alerting.existingSecret configured and
# asserts that the webhook URL is injected from a secretKeyRef rather than
# from the ConfigMap.
#
# Usage:
#   bash scripts/alerting-integration.sh
#
# Exit codes:
#   0  All checks passed
#   1  A check failed or a required tool is missing

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
cd "${REPO_ROOT}"

log()  { echo "[ alerting-integration ] $*"; }
fail() { echo "[ alerting-integration ] FAIL: $*" >&2; exit 1; }

# ── Prerequisites ─────────────────────────────────────────────────────────────

command -v go   >/dev/null 2>&1 || fail "go not found on PATH"
command -v helm >/dev/null 2>&1 || fail "helm not found on PATH"

log "Go version: $(go version)"
log "Helm version: $(helm version --short)"

# ── Go integration tests ──────────────────────────────────────────────────────
#
# The tests exercise every scenario in the mission brief:
#   A  Firing — real IFA findings cause automatic webhook deliveries.
#   B  Deduplication — persistent conditions send exactly one firing event.
#   C  Resolution — clearing the condition sends a resolved event.
#   D  Escalation — same-ID severity transitions fire an escalated event.
#   E  Severity filtering — findings below min_severity are silently dropped.
#      + HTTP failure modes (400/401/429/500/503/timeout/refused/redirect)
#      + Queue saturation and recovery
#      + Graceful shutdown with queued alerts
#      + Restart re-fires open alerts
#      + Prometheus self-metrics reflect actual delivery outcomes
#      + Credential redaction in log output

log "running alerting integration tests …"
go test \
  -tags integration \
  -race \
  -count=1 \
  -timeout 180s \
  -v \
  ./internal/alerting/... 2>&1 | tee /tmp/alerting-integration-go.log

log "Go integration tests passed"

# ── Helm Secret wiring ────────────────────────────────────────────────────────
#
# The webhook URL contains the credential (Slack, Teams, and PagerDuty all
# embed it in the path). It must never appear in a ConfigMap.

log "checking Helm alerting Secret wiring …"

CONFIGMAP=$(helm template ifa deploy/helm/ifa \
  --set alerting.existingSecret=ifa-alerting \
  --set config.alerting.enabled=true \
  | awk '/kind: ConfigMap/{found=1} found{print; if(/^---/)exit}')

# A populated webhook URL in the ConfigMap would look like
#   webhook_url: "http://..." or webhook_url: https://...
if echo "${CONFIGMAP}" | grep -qE 'webhook_url:\s+"?(https?://)'; then
  echo "FAIL: ConfigMap contains a populated webhook_url"
  exit 1
fi
log "✓ ConfigMap has no populated webhook_url"

DEPLOY=$(helm template ifa deploy/helm/ifa \
  --set alerting.existingSecret=ifa-alerting \
  --set config.alerting.enabled=true \
  | awk '/kind: Deployment/{found=1} found{print; if(/^---/)exit}')

if ! echo "${DEPLOY}" | grep -q "IFA_ALERTING_WEBHOOK_URL"; then
  echo "FAIL: Deployment does not inject IFA_ALERTING_WEBHOOK_URL"
  exit 1
fi
if ! echo "${DEPLOY}" | grep -q "secretKeyRef"; then
  echo "FAIL: Deployment does not use secretKeyRef for IFA_ALERTING_WEBHOOK_URL"
  exit 1
fi
log "✓ Deployment uses secretKeyRef for IFA_ALERTING_WEBHOOK_URL"

# When no existingSecret is set, alerting is disabled so no env var is emitted.
DEPLOY_NOALERT=$(helm template ifa deploy/helm/ifa \
  | awk '/kind: Deployment/{found=1} found{print; if(/^---/)exit}')

if echo "${DEPLOY_NOALERT}" | grep -qE 'IFA_ALERTING_WEBHOOK_URL.*secretKeyRef'; then
  echo "FAIL: secretKeyRef appears when alerting.existingSecret is not set"
  exit 1
fi
log "✓ No secretKeyRef when alerting.existingSecret is unset"

# ── Summary ───────────────────────────────────────────────────────────────────

log ""
log "═══════════════════════════════════════════════════════════════════════"
log " Alerting integration: ALL CHECKS PASSED"
log ""
log " Scenarios verified:"
log "   A  Firing via automatic OnCycle evaluation (not via API)"
log "   B  Deduplication across multiple collection cycles"
log "   C  Resolution when the underlying condition clears"
log "   D  Escalation (sender layer; no built-in rule changes severity in-place)"
log "   E  Severity filtering (min_severity=warning and critical)"
log "   +  HTTP 400/401/403/404/429/500/503 failure handling"
log "   +  Timeout and connection-refused transport failures"
log "   +  Redirect not followed (token isolation)"
log "   +  Failed delivery state tracking"
log "   +  Queue saturation and recovery"
log "   +  Graceful shutdown drain"
log "   +  Slow-receiver shutdown boundedness"
log "   +  Restart re-fires open conditions"
log "   +  Prometheus self-metrics (5 alert counters/gauges)"
log "   +  Token never appears in log output"
log "   +  Helm secretKeyRef wiring"
log "═══════════════════════════════════════════════════════════════════════"
