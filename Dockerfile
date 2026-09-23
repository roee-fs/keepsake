# --platform=$BUILDPLATFORM, not the target: the release builds linux/amd64 and
# linux/arm64, so without this the identical bundle is built twice, once emulated.
FROM --platform=$BUILDPLATFORM oven/bun:1 AS ui
WORKDIR /ui
COPY frontend/package.json frontend/bun.lock ./
RUN bun install --frozen-lockfile
COPY frontend ./
RUN bun run build

# The build platform as well: Go cross-compiles to the target without emulation.
FROM --platform=$BUILDPLATFORM golang:1.27@sha256:3680233e3204827fbdc66088528ae6d4b3d034f51d03a99d454f6de034888244 AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
# Only what the binary compiles or embeds, so a docs or chart edit does not rebuild it.
COPY cmd cmd
COPY okf okf
COPY internal internal
COPY frontend/embed.go frontend/openapi.json frontend/
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags "-s -w" -o /keepsake ./cmd/keepsake

FROM gcr.io/distroless/static-debian12@sha256:d75cdd72874d4790092fcb1b058493ecf6bb5bf2b2b897045b00ff01d91843f2
COPY --from=build /keepsake /usr/local/bin/keepsake
COPY --from=ui /ui/dist /app/static
# Numeric, not the name `nobody`: the kubelet cannot resolve a name to a UID, so a
# pod asking for runAsNonRoot refuses to start rather than running unprivileged.
USER 65534
ENTRYPOINT []
CMD ["keepsake", "serve"]
