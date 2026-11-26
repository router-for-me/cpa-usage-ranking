# syntax=docker/dockerfile:1

ARG NODE_IMAGE=node:24.22.1-alpine
ARG GO_IMAGE=golang:1.26.10-bookworm
ARG RUNTIME_IMAGE=alpine:3.24

# All build stages run on the builder's architecture; the final stage has no
# RUN instructions and can therefore be assembled without target binfmt.
FROM --platform=$BUILDPLATFORM ${NODE_IMAGE} AS web-builder
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
COPY THIRD_PARTY_NOTICES.md /src/THIRD_PARTY_NOTICES.md
COPY third_party/react-bits/LICENSE.md /src/third_party/react-bits/LICENSE.md
RUN npm run build

FROM --platform=$BUILDPLATFORM ${GO_IMAGE} AS go-builder
WORKDIR /src
# The dependency also keeps the two resource-intensive builds sequential.
COPY --from=web-builder /src/web/dist /out/web
ARG BUILDARCH
ARG TARGETARCH
RUN case "${BUILDARCH}/${TARGETARCH}" in \
      amd64/amd64|arm64/arm64) cross_packages='' ;; \
      amd64/arm64) dpkg --add-architecture arm64; cross_packages='gcc-aarch64-linux-gnu musl-dev:arm64' ;; \
      arm64/amd64) dpkg --add-architecture amd64; cross_packages='gcc-x86-64-linux-gnu musl-dev:amd64' ;; \
      *) echo "unsupported build/target architecture: ${BUILDARCH}/${TARGETARCH}" >&2; exit 1 ;; \
    esac && \
    apt-get update && apt-get install -y --no-install-recommends musl-tools ${cross_packages} && \
    rm -rf /var/lib/apt/lists/*
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ranking/ ./cmd/ranking/
COPY internal/ ./internal/
RUN case "${BUILDARCH}/${TARGETARCH}" in \
      amd64/arm64) export CC='aarch64-linux-gnu-gcc -specs=/usr/lib/aarch64-linux-musl/musl-gcc.specs' ;; \
      arm64/amd64) export CC='x86_64-linux-gnu-gcc -specs=/usr/lib/x86_64-linux-musl/musl-gcc.specs' ;; \
      *) export CC=musl-gcc ;; \
    esac && \
    CGO_ENABLED=1 GOOS=linux GOARCH="${TARGETARCH}" GOMAXPROCS=2 \
      go build -p 2 -tags 'netgo osusergo' \
      -ldflags '-s -w -linkmode external -extldflags "-static"' \
      -o /out/ranking ./cmd/ranking && \
    mkdir -m 0700 /out/data

FROM ${RUNTIME_IMAGE}
WORKDIR /app
COPY --from=go-builder /out/ranking /app/ranking
COPY --from=go-builder /out/web/ /app/web/
COPY --from=go-builder --chown=10001:10001 --chmod=0700 /out/data/ /data/
COPY config/models.json /app/config/models.json
ENV RANKING_HTTP_ADDR=0.0.0.0:8080 \
    RANKING_SQLITE_PATH=/data/ranking.db \
    RANKING_WEB_DIR=/app/web \
    RANKING_MODELS_PATH=/app/config/models.json \
    RANKING_HEALTHCHECK_URL=http://127.0.0.1:8080/healthz
USER 10001:10001
VOLUME ["/data"]
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=3s --start-period=15s --retries=3 \
    CMD wget -q -T 2 -O /dev/null "$RANKING_HEALTHCHECK_URL" || exit 1
ENTRYPOINT ["/app/ranking"]
