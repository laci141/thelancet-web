# syntax=docker/dockerfile:1
#
# thelancet-web: a Go wrapper that serves the UI and the /affiliations + /authors
# endpoints (shelling out to the thelancet analytics commands against a local
# mirror DB). The mirror DB (data.db, ~117 MB) is SHIPPED WITH THE REPO via Git
# LFS — a full Lancet-family history (35k works, 2005-2026) so the UI shows real
# growth% instead of "New" badges. No build-time refresh, no network needed at build.
# IMPORTANT: Render must be configured to pull LFS objects (see DEPLOY.md).

# ---- Stage 1: build the web wrapper for linux/amd64 -------------------------
FROM golang:1.26-alpine AS web-builder
WORKDIR /build
COPY go.mod ./
COPY main.go ./
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o /out/server ./main.go

# ---- Stage 2: runtime, with the shipped LFS mirror --------------------------
FROM alpine:latest
# ca-certificates: analytics may call OpenAlex over HTTPS for live fallback.
RUN apk add --no-cache ca-certificates
WORKDIR /app
COPY --from=web-builder /out/server ./server
COPY bin/thelancet-pp-cli-linux ./thelancet
COPY index.html ./index.html
# Ship the pre-built mirror DB (via Git LFS) instead of refreshing at build time.
COPY data.db ./data.db
RUN chmod +x ./thelancet ./server && chmod 644 /app/data.db
ENV CLI_BIN=/app/thelancet
ENV THELANCET_DB=/app/data.db
# Wrapper binds 0.0.0.0:$PORT when Render sets $PORT; locally defaults to :8080.
EXPOSE 8080
CMD ["./server"]
