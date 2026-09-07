# syntax=docker/dockerfile:1.7

ARG GO_IMAGE=golang:1.25.14-alpine
ARG NODE_IMAGE=node:24-alpine
ARG RUNTIME_IMAGE=gcr.io/distroless/static-debian12:nonroot

FROM --platform=$BUILDPLATFORM ${NODE_IMAGE} AS frontend
WORKDIR /src

COPY web/package.json web/package-lock.json ./web/
RUN cd web && npm ci
COPY web ./web
RUN cd web && npm run build

FROM --platform=$BUILDPLATFORM ${GO_IMAGE} AS build
WORKDIR /src/sparkroute

COPY go.mod go.sum ./
RUN GOWORK=off go mod download
COPY . ./
COPY --from=frontend /src/pkg/adminui/ui ./pkg/adminui/ui

ARG VERSION=0.1.0-dev
ARG COMMIT=development
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build \
      -trimpath \
      -ldflags="-s -w -buildid= -X github.com/sparksq/sparkroute/pkg/version.Version=${VERSION} -X github.com/sparksq/sparkroute/pkg/version.Commit=${COMMIT}" \
      -o /out/sparkroute \
      ./cmd/sparkroute

FROM ${RUNTIME_IMAGE}
ARG VERSION=0.1.0-dev
ARG COMMIT=development
LABEL org.opencontainers.image.title="SparkRoute" \
      org.opencontainers.image.licenses="AGPL-3.0-only" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${COMMIT}" \
      org.opencontainers.image.source="https://github.com/sparksq/sparkroute"

# The command keeps loopback-safe defaults for direct host execution; the image
# opts its data and operations listeners into the container network. The default
# standalone command explicitly confines admin to container loopback; the
# cluster profile retains its independently bound :8081 admin default.
ENV SPARKROUTE_DATA_ADDRESS=0.0.0.0:8080 \
    SPARKROUTE_OPERATIONS_ADDRESS=0.0.0.0:9090

COPY --from=build /out/sparkroute /sparkroute
COPY LICENSE NOTICE COPYRIGHT THIRD_PARTY_NOTICES.md /licenses/
COPY LICENSES /licenses/LICENSES/

EXPOSE 8080 9090
USER 65532:65532
STOPSIGNAL SIGTERM
ENTRYPOINT ["/sparkroute"]
CMD ["-admin-address=127.0.0.1:8081"]
