# ---- build ------------------------------------------------------------------
FROM golang:1.25-alpine AS build

WORKDIR /src

# Dependencies are copied first so a source-only change reuses the module layer.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO is not needed: pgx is pure Go, which keeps the runtime image minimal.
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

# Media lives on a volume; the directory is created up front so the mount
# inherits the right ownership.
RUN mkdir -p /app/storage/media && chown -R app:app /app/storage

USER app

ENV APP_ENV=production \
    PORT=8080 \
    WEB_DIR=/app/web \
    MEDIA_STORAGE_PATH=/app/storage/media \
    TIMEZONE=Asia/Almaty

EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 \
    CMD curl -fsS http://localhost:8080/api/health || exit 1

ENTRYPOINT ["server"]
