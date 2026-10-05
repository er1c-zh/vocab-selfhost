# Build the responsive PWA first.
FROM node:24-alpine AS web-build
WORKDIR /src/web
COPY web/package*.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

# Compile a static Go API binary with the built UI embedded in it.
FROM golang:1.26-alpine AS api-build
WORKDIR /src/api
COPY api/go.mod ./
RUN go mod download
COPY api/ ./
COPY --from=web-build /src/web/dist ./web/dist
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/vocab-api .

# Runtime image contains the API and its UI; SQLite data lives in /data.
FROM alpine:3.22 AS runtime
RUN apk add --no-cache ca-certificates tzdata
WORKDIR /app
COPY --from=api-build /out/vocab-api /app/vocab-api
RUN mkdir -p /data /audio && chown -R 10001:10001 /data /audio /app
USER 10001:10001
ENV APP_ADDR=:8080 APP_DATA_DIR=/data AUDIO_DIR=/audio
VOLUME ["/data", "/audio"]
EXPOSE 8080
ENTRYPOINT ["/app/vocab-api"]

