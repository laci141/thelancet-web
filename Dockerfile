# syntax=docker/dockerfile:1
#
# thelancet-web: a Go wrapper that serves the UI and the /affiliations + /authors
# endpoints (shelling out to the thelancet analytics commands against a local
# mirror DB). The mirror is built AT IMAGE BUILD TIME with `thelancet refresh`
# (bounded) — the 106 MB local dev DB is deliberately NOT shipped; the build
# fetches a fresh, small snapshot instead. Build requires network to OpenAlex.

# ---- Stage 1: build the web wrapper for linux/amd64 -------------------------
FROM golang:1.26-alpine AS web-builder
WORKDIR /build
COPY go.mod ./
COPY main.go ./
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o /out/server ./main.go

# ---- Stage 2: runtime, with a freshly-refreshed mirror ----------------------
FROM alpine:latest
# ca-certificates: refresh + analytics call OpenAlex over HTTPS.
RUN apk add --no-cache ca-certificates
WORKDIR /app
COPY --from=web-builder /out/server ./server
COPY bin/thelancet-pp-cli-linux ./thelancet
COPY index.html ./index.html
RUN chmod +x ./thelancet ./server && \
    ./thelancet refresh --db /app/data.db --journal lancet --years 3 --max-pages 2 && \
    chmod 644 /app/data.db
ENV CLI_BIN=/app/thelancet
ENV THELANCET_DB=/app/data.db
# Wrapper binds 0.0.0.0:$PORT when Render sets $PORT; locally defaults to :8080.
EXPOSE 8080
CMD ["./server"]
