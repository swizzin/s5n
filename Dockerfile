# syntax=docker/dockerfile:1
# One image for the wiring Job, the qBittorrent credential seed and (later) box.
FROM golang:1.27-alpine AS build
WORKDIR /src
# The module cache and the compiler's own cache are BuildKit cache mounts, not
# layers: `COPY . .` below invalidates the build on any source change, and a
# fresh module graph downloads again on any go.mod change, so without these a
# `docker build` of an edited tree recompiles from cold -- which every e2e run
# starts with. Neither mount changes the image that is produced. The release
# workflow builds this with `docker buildx build` (linux/amd64 + linux/arm64)
# after setup-buildx-action, which is BuildKit too.
COPY go.mod ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download
COPY . .
ARG VERSION=dev
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/s5n ./cmd/s5n

FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/s5n /usr/local/bin/s5n
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/s5n"]
