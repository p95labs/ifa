#!/usr/bin/env bash
#
# package-offline.sh — produce everything needed to install IFA in a cluster
# with no internet access: the container image as a tarball, the packaged Helm
# chart, and checksums.
#
# Run on a connected machine, copy the output, then on the target:
#
#   docker load -i ifa-image-<version>.tar     # or: ctr -n k8s.io images import
#   helm install ifa ifa-<version>.tgz \
#     --namespace inference --create-namespace \
#     --set image.repository=<your-registry>/ifa \
#     --set image.tag=<version>
#
# IFA makes no outbound connections of its own — no licence check, no telemetry,
# no update check — so nothing else needs allowing through.

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${ROOT}"

VERSION="${1:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"
IMAGE="${IMAGE:-ghcr.io/p95labs/ifa}"
OUT="${OUT:-dist}"

for tool in docker helm; do
  if ! command -v "${tool}" >/dev/null 2>&1; then
    echo "error: ${tool} is required" >&2
    exit 1
  fi
done

mkdir -p "${OUT}"

echo "==> Building ${IMAGE}:${VERSION}"
docker build -f deploy/docker/Dockerfile --build-arg "VERSION=${VERSION}" \
  -t "${IMAGE}:${VERSION}" .

echo "==> Saving image"
docker save "${IMAGE}:${VERSION}" -o "${OUT}/ifa-image-${VERSION}.tar"

echo "==> Packaging Helm chart"
helm package deploy/helm/ifa --version "${VERSION#v}" --app-version "${VERSION#v}" \
  --destination "${OUT}" >/dev/null

echo "==> Checksums"
if command -v sha256sum >/dev/null 2>&1; then
  ( cd "${OUT}" && sha256sum ./* > SHA256SUMS )
else
  ( cd "${OUT}" && shasum -a 256 ./* > SHA256SUMS )
fi

echo
echo "Wrote:"
ls -1 "${OUT}"
