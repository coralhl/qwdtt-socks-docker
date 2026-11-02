ARG QWDTT_VERSION=v1.3.0

FROM golang:1.26-alpine AS build
ARG QWDTT_VERSION
RUN apk add --no-cache tzdata
# Client sources come from the pinned GitHub release tag.
RUN wget -qO /tmp/src.tar.gz "https://github.com/code2fingers/qwdtt-openwrt/archive/refs/tags/${QWDTT_VERSION}.tar.gz" \
    && mkdir -p /src && tar xzf /tmp/src.tar.gz -C /src --strip-components=1
WORKDIR /src/client
# No -tags=openwrt: the full build keeps userspace WireGuard and the SOCKS5 server.
RUN go mod download && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o /out/qwdtt-client .

# statusd is local (not part of the upstream repo).
WORKDIR /src/statusd
COPY statusd/ ./
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w -X main.clientVersion=${QWDTT_VERSION}" -o /out/statusd .
# Writable work directory for the unprivileged runtime user.
RUN mkdir -p /out/work && chown 65534:65534 /out/work

# Runtime stage: alpine so the notifier script can run (sh + curl).
FROM alpine:3.22
RUN apk add --no-cache ca-certificates curl
COPY --from=build /usr/share/zoneinfo /usr/share/zoneinfo
COPY --from=build /out/qwdtt-client /qwdtt-client
COPY --from=build /out/statusd /statusd
COPY --chmod=0755 notify.sh /usr/local/bin/notify
COPY --from=build --chown=65534:65534 /out/work /work
WORKDIR /work
USER 65534
ENTRYPOINT ["/statusd"]
