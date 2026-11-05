#!/usr/bin/env bash
#
# verify-release.sh — verify the signature and SBOM for a published IFA image.
#
# Usage:
#   bash scripts/verify-release.sh ghcr.io/p95labs/ifa@sha256:<digest>
#   bash scripts/verify-release.sh ghcr.io/p95labs/ifa:v0.3.0
#
# Requirements: cosign, jq (optional for pretty-printing)
#
# Images published before the signing workflow was introduced (prior to
# v0.3.0) carry no signature; this script will report an error for them.

set -euo pipefail

IMAGE="${1:-}"
if [[ -z "${IMAGE}" ]]; then
    echo "Usage: $0 <image-ref>" >&2
    echo "Example: $0 ghcr.io/p95labs/ifa@sha256:<digest>" >&2
    exit 1
fi

OIDC_ISSUER="https://token.actions.githubusercontent.com"
WORKFLOW_IDENTITY="https://github.com/p95labs/ifa/.github/workflows/release.yml"

if ! command -v cosign > /dev/null 2>&1; then
    echo "ERROR: cosign is not installed." >&2
    echo "Install: https://docs.sigstore.dev/cosign/system_config/installation/" >&2
    exit 1
fi

echo "==> Verifying signature for: ${IMAGE}"
if cosign verify \
    --certificate-identity-regexp "${WORKFLOW_IDENTITY}" \
    --certificate-oidc-issuer "${OIDC_ISSUER}" \
    "${IMAGE}" 2>/dev/null | (command -v jq > /dev/null && jq -r '
      .[0].optional |
      "  Issuer:   \(.Issuer)\n  Subject:  \(.Subject)\n  Workflow: \(.Workflow // "n/a")"
    ' || cat); then
    echo "PASS  signature verified"
else
    echo "FAIL  signature verification failed" >&2
    echo "      Images before v0.3.0 carry no signature." >&2
    exit 1
fi

echo ""
echo "==> Verifying SBOM attestation for: ${IMAGE}"
if cosign verify-attestation \
    --type spdxjson \
    --certificate-identity-regexp "${WORKFLOW_IDENTITY}" \
    --certificate-oidc-issuer "${OIDC_ISSUER}" \
    "${IMAGE}" > /tmp/ifa-sbom-attestation.json 2>/dev/null; then
    echo "PASS  SBOM attestation verified"
    SBOM_FILE=/tmp/ifa-sbom-${RANDOM}.spdx.json
    jq -r '.payload' /tmp/ifa-sbom-attestation.json \
        | base64 -d \
        | jq -r '.predicate' \
        > "${SBOM_FILE}" 2>/dev/null || true
    if [[ -s "${SBOM_FILE}" ]]; then
        PKG_COUNT=$(jq '.packages | length' "${SBOM_FILE}" 2>/dev/null || echo "unknown")
        echo "      SPDX document: ${SBOM_FILE}"
        echo "      Package count: ${PKG_COUNT}"
    fi
else
    echo "FAIL  SBOM attestation verification failed" >&2
    exit 1
fi

echo ""
echo "Both signature and SBOM attestation verified successfully."
