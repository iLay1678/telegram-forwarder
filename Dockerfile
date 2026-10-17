# syntax=docker/dockerfile:1
ARG GO_VERSION=1.26.4
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS build

WORKDIR /src
ENV CGO_ENABLED=0 GOGC=30 GOMEMLIMIT=2GiB GOFLAGS="-p=1"
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY cmd ./cmd
COPY internal ./internal
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG COMMIT=unknown
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath \
    -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT}" \
    -o /out/telegram-forwarder ./cmd/telegram-forwarder

FROM alpine:3.22
RUN apk add --no-cache ca-certificates tzdata \
    && mkdir -p /app/db /app/logs /app/sessions /app/temp \
       /app/ufb/config /app/config /app/rss/data /app/rss/media
WORKDIR /app
ENV TZ=Asia/Shanghai
COPY --from=build /out/telegram-forwarder /app/telegram-forwarder
COPY LICENSE /app/LICENSE
EXPOSE 8000
HEALTHCHECK --interval=30s --timeout=5s --start-period=90s --retries=3 \
    CMD ["/app/telegram-forwarder", "--healthcheck"]
ENTRYPOINT ["/app/telegram-forwarder"]
