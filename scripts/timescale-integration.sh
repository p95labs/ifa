#!/usr/bin/env bash
# scripts/timescale-integration.sh
#
# Start a real TimescaleDB container, apply the migration, run the
# integration-tagged tests, and clean up — all on free local infrastructure.
#
# Usage:
#   bash scripts/timescale-integration.sh
#
# Requirements:
#   • Docker (any recent version with the `docker` CLI on PATH)
#   • Go (same version as CI, see GO_VERSION in .github/workflows/ci.yml)
#
# The TimescaleDB image used is timescale/timescaledb:2.14.2-pg16, the same
# version as in CI. Pin it to reproduce a specific result:
#   TSDB_IMAGE=timescale/timescaledb:2.14.2-pg16 bash scripts/timescale-integration.sh
#
# Exit codes:
#   0  All integration tests passed
#   1  Tests failed or prerequisite missing

set -euo pipefail

TSDB_IMAGE="${TSDB_IMAGE:-timescale/timescaledb:2.14.2-pg16}"
CONTAINER_NAME="ifa-tsdb-integration-$$"
DB_NAME="ifa_test"
DB_USER="ifa_test"
DB_PASS="ifa_test"
DB_PORT="${TSDB_PORT:-5433}"  # 5433 to avoid clashing with a local Postgres

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

# ── pre-flight ────────────────────────────────────────────────────────────────

log()  { echo "[ timescale-integration ] $*"; }
fail() { echo "[ timescale-integration ] FAIL: $*" >&2; exit 1; }

command -v docker >/dev/null 2>&1 || fail "docker not found on PATH"
command -v go     >/dev/null 2>&1 || fail "go not found on PATH"

cd "${REPO_ROOT}"

# ── container lifecycle ───────────────────────────────────────────────────────

cleanup() {
  log "removing container ${CONTAINER_NAME}"
  docker rm -f "${CONTAINER_NAME}" >/dev/null 2>&1 || true
}
trap cleanup EXIT

log "pulling ${TSDB_IMAGE} …"
docker pull "${TSDB_IMAGE}" --quiet

log "starting TimescaleDB container ${CONTAINER_NAME} on port ${DB_PORT}"
docker run -d \
  --name "${CONTAINER_NAME}" \
  -e "POSTGRES_DB=${DB_NAME}" \
  -e "POSTGRES_USER=${DB_USER}" \
  -e "POSTGRES_PASSWORD=${DB_PASS}" \
  -p "${DB_PORT}:5432" \
  "${TSDB_IMAGE}" >/dev/null

DSN="postgres://${DB_USER}:${DB_PASS}@127.0.0.1:${DB_PORT}/${DB_NAME}?sslmode=disable"

# ── wait for readiness ────────────────────────────────────────────────────────

log "waiting for TimescaleDB to accept connections …"
READY=0
for _ in $(seq 1 30); do
  if docker exec "${CONTAINER_NAME}" \
       pg_isready -U "${DB_USER}" -d "${DB_NAME}" >/dev/null 2>&1; then
    READY=1
    break
  fi
  sleep 2
done
[[ ${READY} -eq 1 ]] || fail "TimescaleDB did not become ready in 60s"
log "TimescaleDB ready"

# ── apply migration ───────────────────────────────────────────────────────────

log "applying migrations/001_init.sql …"
docker exec -i "${CONTAINER_NAME}" \
  psql -U "${DB_USER}" -d "${DB_NAME}" \
  < "${REPO_ROOT}/migrations/001_init.sql"
log "migration applied"

# ── SQL sanity check ──────────────────────────────────────────────────────────

log "verifying hypertable, columns, and index …"

HYPER_COUNT=$(docker exec "${CONTAINER_NAME}" psql -U "${DB_USER}" -d "${DB_NAME}" -tA \
  -c "SELECT count(*) FROM timescaledb_information.hypertables WHERE hypertable_name='telemetry_snapshots'")
[[ "${HYPER_COUNT}" == "1" ]] || fail "telemetry_snapshots is not a hypertable (count=${HYPER_COUNT})"
log "  ✓ telemetry_snapshots is a hypertable"

IDX_COUNT=$(docker exec "${CONTAINER_NAME}" psql -U "${DB_USER}" -d "${DB_NAME}" -tA \
  -c "SELECT count(*) FROM pg_indexes WHERE tablename='telemetry_snapshots' AND indexname='idx_telemetry_workload'")
[[ "${IDX_COUNT}" == "1" ]] || fail "idx_telemetry_workload not found (count=${IDX_COUNT})"
log "  ✓ idx_telemetry_workload exists"

COL_COUNT=$(docker exec "${CONTAINER_NAME}" psql -U "${DB_USER}" -d "${DB_NAME}" -tA \
  -c "SELECT count(*) FROM information_schema.columns WHERE table_name='telemetry_snapshots'")
# The schema has 28 columns (6 identity + 22 measurements).
[[ "${COL_COUNT}" -ge 28 ]] || \
  fail "expected ≥28 columns in telemetry_snapshots, got ${COL_COUNT}"
log "  ✓ ${COL_COUNT} columns present (expected ≥28)"

# ── run integration tests ─────────────────────────────────────────────────────

log "running integration tests (this may take ~30 s) …"
TIMESCALE_TEST_DSN="${DSN}" \
  go test -tags integration -race -count=1 -v \
  -timeout 120s \
  ./internal/storage/timescale/...

log "all integration tests passed"

# ── Helm ConfigMap credential check ──────────────────────────────────────────

log "verifying Helm ConfigMap does not contain DSN when database.existingSecret is set …"
CONFIGMAP=$(helm template ifa "${REPO_ROOT}/deploy/helm/ifa" \
  --set database.existingSecret=ifa-db \
  --set config.database.enabled=true \
  | grep -A9999 'kind: ConfigMap' | head -60)

# The key `dsn:` is expected with an empty value (""). A real credential would
# start with postgres:// or look like a URI. Check for a non-empty DSN value
# specifically, and for any literal `postgres://` URL anywhere in the ConfigMap.
if echo "${CONFIGMAP}" | grep -qE 'postgres://'; then
  fail "Helm ConfigMap contains a postgres:// DSN — credential leaked"
fi
# dsn: ""  is safe; dsn: "postgres://..." is not
if echo "${CONFIGMAP}" | grep -qE 'dsn:\s+"[^"]'; then
  fail "Helm ConfigMap database.dsn has a non-empty value — credential leaked"
fi
log "  ✓ ConfigMap has no populated postgres:// DSN"

# Confirm dsn key is present but empty.
if ! echo "${CONFIGMAP}" | grep -qE 'dsn:\s*""'; then
  fail "ConfigMap database.dsn is not present/empty as expected"
fi
log "  ✓ ConfigMap database.dsn is empty (credential injected via Secret)"

DEPLOYMENT=$(helm template ifa "${REPO_ROOT}/deploy/helm/ifa" \
  --set database.existingSecret=ifa-db \
  --set config.database.enabled=true \
  | grep -A9999 'kind: Deployment')

if ! echo "${DEPLOYMENT}" | grep -q "secretKeyRef"; then
  fail "Deployment does not use secretKeyRef for database DSN"
fi
log "  ✓ Deployment uses secretKeyRef for IFA_DATABASE_DSN"

log ""
log "TimescaleDB integration validation complete."
log ""
log "  Image:    ${TSDB_IMAGE}"
log "  PG ver:   $(docker exec "${CONTAINER_NAME}" psql -U "${DB_USER}" -d "${DB_NAME}" -tA -c 'SHOW server_version')"
log "  TSDB ext: $(docker exec "${CONTAINER_NAME}" psql -U "${DB_USER}" -d "${DB_NAME}" -tA \
     -c "SELECT extversion FROM pg_extension WHERE extname='timescaledb'")"
