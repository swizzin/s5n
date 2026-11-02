# syntax=docker/dockerfile:1
# One image for the wiring Job, the qBittorrent credential seed and (later) box.
FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/s5n ./cmd/s5n

FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/s5n /usr/local/bin/s5n
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/s5n"]
