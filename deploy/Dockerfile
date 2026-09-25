# MockVision node image: one static binary with the panel embedded, FFmpeg
# to encode the camera streams, and nothing else. Run it with the
# compose.yaml at the root of the repository: host network, only the
# capabilities of the network helper, read-only root filesystem.
#
#   docker build -f deploy/Dockerfile -t mockvision .

ARG GO_VERSION=1.27
ARG NODE_VERSION=22

# The panel is plain static files: it is built once for every platform.
FROM --platform=$BUILDPLATFORM node:${NODE_VERSION}-slim AS panel
WORKDIR /src/frontend
COPY frontend/package.json frontend/package-lock.json ./
RUN --mount=type=cache,target=/root/.npm npm ci --no-audit --no-fund
COPY frontend/ ./
RUN npm run build

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION} AS binary
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=0.1.0
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
COPY --from=panel /src/frontend/dist ./frontend/dist
# CGO_ENABLED=0 is required: dropping privileges changes every thread at once.
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
    -ldflags "-s -w -X github.com/corticoide/mockvision/backend/internal/buildinfo.Version=$VERSION" \
    -o /out/mockvision ./backend/cmd/mockvision

FROM ubuntu:24.04
RUN apt-get update \
    && apt-get install -y --no-install-recommends ffmpeg ca-certificates iproute2 \
    && rm -rf /var/lib/apt/lists/*
# The main service and the cameras run as two different unprivileged users,
# so a camera can never read the database or the node key.
RUN groupadd --system --gid 10001 mockvision \
    && useradd --system --uid 10001 --gid 10001 --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin mockvision \
    && groupadd --system --gid 10002 mockvision-cam \
    && useradd --system --uid 10002 --gid 10002 --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin mockvision-cam
COPY --from=binary /out/mockvision /usr/local/bin/mockvision
ENV MOCKVISION_DATA=/data
VOLUME /data
EXPOSE 8080
ENTRYPOINT ["mockvision"]
CMD ["run"]
