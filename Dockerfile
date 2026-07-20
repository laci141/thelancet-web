# syntax=docker/dockerfile:1
#
# thelancet-web: Go wrapper serving the UI + /affiliations + /authors + /drift +
# /curate + /check endpoints against a local mirror DB. The mirror (data.db,
# ~117 MB, full Lancet history 2005-2026, 35k works) is stored via Git LFS.
# Because Render's Docker build does NOT auto-fetch LFS objects, an LFS-aware
# stage pulls the real DB explicitly.
#
# Two CLI binaries ship in the runtime image:
#   - thelancet-pp-cli       (analytics: affiliations, authors, drift, curate)
#   - retraction-checker-pp-cli (live retraction status over Crossref & OpenAlex)

# ---- Stage 1: fetch the LFS data.db from GitHub -----------------------------
FROM alpine/git:latest AS lfs-fetcher
RUN apk add --no-cache git-lfs && git lfs install
WORKDIR /fetch
# Clone the repo and pull LFS objects (gets the real 117 MB data.db, not pointer).
RUN git clone https://github.com/laci141/pubvera-bibliovera.git . && \
    git lfs pull && \
    ls -lh data.db

# ---- Stage 2: build the web wrapper for linux/amd64 -------------------------
FROM golang:1.26-alpine AS web-builder
WORKDIR /build
COPY go.mod ./
COPY main.go ./
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o /out/server ./main.go

# ---- Stage 3: runtime -------------------------------------------------------
FROM alpine:latest
RUN apk add --no-cache ca-certificates
WORKDIR /app
COPY --from=web-builder /out/server ./server
COPY bin/thelancet-pp-cli-linux ./thelancet
COPY bin/retraction-checker-pp-cli-linux ./retraction-checker
COPY index.html ./index.html
# Pull the real LFS DB from the fetcher stage (not the local build context).
COPY --from=lfs-fetcher /fetch/data.db ./data.db
RUN chmod +x ./thelancet ./retraction-checker ./server && chmod 644 /app/data.db
ENV CLI_BIN=/app/thelancet
ENV THELANCET_DB=/app/data.db
ENV RETRACTION_CHECKER_BIN=/app/retraction-checker
EXPOSE 8080
CMD ["./server"]