# The AudioSilo Bridge images (BRIDGE-PLAN.md Phase 0), three targets from one
# file so the web and Go build stages are defined once:
#
#   - slim  : Go + ffmpeg. Enough for every job that doesn't transcribe (verify,
#             and podcasts later): the cheap second container.
#   - full  : slim + a CPU whisper.cpp (whisper-cli), for transcription. The last
#             stage, so a bare `docker build .` builds it.
#   - cuda  : full's job on an NVIDIA GPU (amd64 only): the CUDA whisper-cli on
#             the nvidia/cuda runtime base. Run with `--gpus all`.
#
# None carries an agent CLI (claude, codex): agents run in a separate `agent`
# container that sees only a staging volume (decision 15), which ships with
# Phase 1a. The whisper models aren't baked in either: the Bridge downloads the
# one it needs into /bridge-data/tools on first use.
#
# The whisper-cli builds are audiosilo-sidecars' release assets (public; see
# internal/toolfetch), pinned here by tag and sha256.

ARG WHISPER_TAG=whisper-cpp-v1.9.1-1

# --- the page ------------------------------------------------------------------
FROM --platform=$BUILDPLATFORM node:24-slim AS web
WORKDIR /web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

# --- the binary, with the page embedded ----------------------------------------
FROM --platform=$BUILDPLATFORM golang:1.26 AS build
ARG TARGETOS TARGETARCH
ARG VERSION=0.0.0-docker
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /web/dist ./internal/web/dist
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -tags embedui \
    -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/audiosilo-bridge ./cmd/audiosilo-bridge

# --- whisper-cli, downloaded and checked ----------------------------------------
FROM --platform=$BUILDPLATFORM debian:stable-slim AS whisper
ARG WHISPER_TAG TARGETARCH
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /w
# cpu for each arch, and the CUDA build for amd64 (its runtime libraries sit
# beside the binary, found through an $ORIGIN RPATH).
RUN set -eu; base="https://github.com/KodeStar/audiosilo-sidecars/releases/download/${WHISPER_TAG}"; \
    curl -fsSLo checksums.txt "$base/checksums.txt"; \
    fetch() { \
      curl -fsSLo "$1" "$base/$1"; \
      grep " $1\$" checksums.txt | sha256sum -c -; \
      mkdir -p "out/$2"; tar -xzf "$1" -C "out/$2"; chmod 0755 "out/$2/whisper-cli"; \
    }; \
    fetch "whisper-cli-linux-${TARGETARCH}-cpu.tar.gz" cpu; \
    if [ "$TARGETARCH" = amd64 ]; then fetch whisper-cli-linux-amd64-cuda.tar.gz cuda; else mkdir -p out/cuda; fi

# --- slim ------------------------------------------------------------------------
# debian-slim (not distroless) so the Bridge can exec ffmpeg/ffprobe; with them on
# $PATH it never downloads them.
FROM debian:stable-slim AS slim
RUN apt-get update \
    && apt-get install -y --no-install-recommends ffmpeg ca-certificates \
    && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/audiosilo-bridge /usr/local/bin/audiosilo-bridge
COPY docker/entrypoint.sh /usr/local/bin/docker-entrypoint.sh
ENV AUDIOSILO_BRIDGE_DATA=/bridge-data \
    PUID=1000 \
    PGID=1000
VOLUME /bridge-data
EXPOSE 8091
ENTRYPOINT ["docker-entrypoint.sh"]
CMD ["audiosilo-bridge", "serve"]

# --- cuda (amd64) ------------------------------------------------------------------
# nvidia/cuda's runtime base (not devel): the whisper build bundles its CUDA
# libraries, and libcuda + nvidia-smi come from the NVIDIA container toolkit at
# run time.
FROM nvidia/cuda:12.6.3-runtime-ubuntu22.04 AS cuda
RUN apt-get update \
    && apt-get install -y --no-install-recommends ffmpeg ca-certificates \
    && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/audiosilo-bridge /usr/local/bin/audiosilo-bridge
COPY --from=whisper /w/out/cuda /opt/whisper
COPY docker/entrypoint.sh /usr/local/bin/docker-entrypoint.sh
ENV AUDIOSILO_BRIDGE_DATA=/bridge-data \
    AUDIOSILO_BRIDGE_ASR_BACKEND=whisper-cpp \
    AUDIOSILO_BRIDGE_ASR_WHISPER_CLI_PATH=/opt/whisper/whisper-cli \
    NVIDIA_VISIBLE_DEVICES=all \
    NVIDIA_DRIVER_CAPABILITIES=compute,utility \
    PUID=1000 \
    PGID=1000
VOLUME /bridge-data
EXPOSE 8091
ENTRYPOINT ["docker-entrypoint.sh"]
CMD ["audiosilo-bridge", "serve"]

# --- full (the default) -------------------------------------------------------------
FROM slim AS full
COPY --from=whisper /w/out/cpu /opt/whisper
ENV AUDIOSILO_BRIDGE_ASR_BACKEND=whisper-cpp \
    AUDIOSILO_BRIDGE_ASR_WHISPER_CLI_PATH=/opt/whisper/whisper-cli
