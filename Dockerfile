# syntax=docker/dockerfile:1
ARG CLOUDFLARED_VERSION=latest

FROM --platform=$BUILDPLATFORM golang:1-alpine AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/tunneld ./cmd/tunneld \
 && mkdir -p /out/data

FROM cloudflare/cloudflared:${CLOUDFLARED_VERSION} AS cloudflared

# distroless/static: CA certs + tzdata + nonroot user, no shell (~2 MB).
FROM gcr.io/distroless/static:nonroot
COPY --from=cloudflared /usr/local/bin/cloudflared /usr/local/bin/cloudflared
COPY --from=build /out/tunneld /usr/local/bin/tunneld
COPY --from=build --chown=65532:65532 /out/data /data
ENV DATA_DIR=/data \
    LISTEN_ADDR=:7070 \
    CLOUDFLARED_BIN=/usr/local/bin/cloudflared \
    DEFAULT_TARGET_HOST=host.docker.internal \
    GOMEMLIMIT=48MiB
VOLUME /data
EXPOSE 7070
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s CMD ["/usr/local/bin/tunneld", "healthcheck"]
ENTRYPOINT ["/usr/local/bin/tunneld"]
