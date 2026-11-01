# Package the output of scripts/build-release.sh on the tdog host.
# Docker only packages precompiled artifacts.
FROM alpine:3.20
LABEL org.opencontainers.image.licenses="AGPL-3.0-only" \
      org.opencontainers.image.source="https://github.com/gao-gao-zai/wk"
RUN apk add --no-cache wget ca-certificates tzdata && adduser -D -u 10001 app && mkdir -p /app/auths /app/data && chown -R app:app /app
ENV WB2A_LISTEN=:7863
WORKDIR /app
COPY wb2api login /app/
COPY frontend /app/frontend
COPY source.tar.gz LICENSE NOTICE /app/
COPY LICENSES /app/LICENSES
RUN chmod 755 /app/frontend /app/frontend/assets && chmod 644 /app/frontend/index.html /app/frontend/assets/*
USER app
EXPOSE 7863
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s CMD wget -qO- http://127.0.0.1:7863/healthz || exit 1
ENTRYPOINT ["/app/wb2api", "-config", "/app/config.json"]
