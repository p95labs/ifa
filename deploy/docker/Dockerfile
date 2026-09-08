# Build both binaries and ship them on a distroless base.
#
# One Dockerfile builds both the control plane and the CLI: the CLI is small,
# and having it in the image means `kubectl exec` is not the only way to inspect
# a running instance from inside the cluster.

FROM golang:1.25-alpine AS build

# git is needed for the VCS stamps the toolchain embeds, which is where the
# build's commit hash in /api/v1/healthz comes from.
RUN apk add --no-cache git ca-certificates

WORKDIR /src

# Dependencies first so a source-only change does not re-download the module
# cache on every build.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build \
        -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
        -o /out/control-plane ./cmd/control-plane && \
    CGO_ENABLED=0 GOOS=linux go build \
        -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
        -o /out/ifa ./cmd/ifa

# distroless static: no shell, no package manager, and nothing to exec into.
# :nonroot runs as uid 65532, which matches the securityContext in the Helm
# chart — a mismatch there is the usual cause of a CreateContainerConfigError.
FROM gcr.io/distroless/static-debian12:nonroot

ARG VERSION=dev
LABEL org.opencontainers.image.title="ifa" \
      org.opencontainers.image.description="Read-only diagnostics for LLM inference workloads on Kubernetes" \
      org.opencontainers.image.source="https://github.com/p95labs/ifa" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.version="${VERSION}"

COPY --from=build /out/control-plane /usr/local/bin/control-plane
COPY --from=build /out/ifa /usr/local/bin/ifa

# The config is mounted from a ConfigMap at this path by the chart.
WORKDIR /etc/ifa
USER 65532:65532
EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/control-plane"]
CMD ["-config", "/etc/ifa/config.yaml"]
