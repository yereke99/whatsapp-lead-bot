# ---- build ------------------------------------------------------------------
FROM golang:1.25-alpine AS build

WORKDIR /src

# Dependencies are copied first so a source-only change reuses the module layer.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO is not needed: the SQLite driver is pure Go, which keeps the runtime
# image minimal and avoids a libsqlite3 dependency.
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build \
        -trimpath \
        -ldflags="-s -w -X main.version=${VERSION}" \
        -o /out/server ./cmd/server \
 && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/migrate ./cmd/migrate \
 && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/seed ./cmd/seed

# ---- runtime ----------------------------------------------------------------
FROM alpine:3.20

# ffmpeg converts uploaded audio into the OGG/Opus form WhatsApp renders as a
# voice note; tzdata provides the IANA zones campaign scheduling depends on.
RUN apk add --no-cache ffmpeg tzdata ca-certificates curl \
 && addgroup -S app \
 && adduser -S -G app -h /app app

WORKDIR /app

COPY --from=build /out/server /out/migrate /out/seed /usr/local/bin/
COPY --chown=app:app web ./web

# The database and media live on volumes; both directories are created up
# front so the mounts inherit the right ownership.
RUN mkdir -p /app/storage/media /app/data \
 && chown -R app:app /app/storage /app/data

USER app

ENV APP_ENV=production \
    PORT=8086 \
    WEB_DIR=/app/web \
    DATABASE_PATH=/app/data/whatsapp.db \
    MEDIA_STORAGE_PATH=/app/storage/media \
    TIMEZONE=Asia/Almaty

EXPOSE 8086

HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 \
    CMD curl -fsS http://localhost:8086/api/health || exit 1

ENTRYPOINT ["server"]
