# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM alpine:3.22 AS runtime-files
RUN apk add --no-cache ca-certificates tzdata \
    && mkdir -p /app/db /app/logs /app/sessions /app/temp \
       /app/ufb/config /app/config /app/rss/data /app/rss/media

FROM alpine:3.22
ARG TARGETOS
ARG TARGETARCH
COPY --from=runtime-files /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=runtime-files /usr/share/zoneinfo /usr/share/zoneinfo
COPY --from=runtime-files /app/ /app/
WORKDIR /app
ENV TZ=Asia/Shanghai
COPY --chmod=0755 dist/${TARGETOS}-${TARGETARCH}/telegram-forwarder /app/telegram-forwarder
COPY LICENSE /app/LICENSE
EXPOSE 8000
HEALTHCHECK --interval=30s --timeout=5s --start-period=90s --retries=3 \
    CMD ["/app/telegram-forwarder", "--healthcheck"]
ENTRYPOINT ["/app/telegram-forwarder"]
