# syntax=docker/dockerfile:1.27.1@sha256:4edf897a3ffa55b89f906fc8cc78afdb3f1834cc9c7083565e611a8a7d5fe99e

# The self-hosting image: the daemon and the open console in one image, built
# from a checkout of this repository. The build context is the checkout's root
# (.dockerignore there lets in only go.mod, go.sum, cmd/, internal/ and web/):
#
#   docker build -f deploy/Dockerfile --build-arg VERSION="$(git describe --always --dirty)" -t mailie:local .
#
# or `make image`, or deploy/compose.yaml. docs/self-hosting.md says how to run
# it.
#
# Three stages. Node builds the open console, Go builds a static binary with
# cgo off, and both land on distroless's static image, run as its nonroot user:
# no shell and no package manager, only CA certificates, time zones and the
# passwd entries beside the binary. Both build stages run on the builder's own
# platform; Go cross-compiles for the target, and the console is the same files
# everywhere.
#
# Every base image is pinned by digest, the tag beside it saying what the
# digest is; bump both together. The Go stage matches go.mod's toolchain and
# CI's GO_VERSION, the Node stage CI's Node 24. Nothing secret is built in:
# configuration comes from the container's environment when it runs.

ARG VERSION=dev

FROM --platform=$BUILDPLATFORM node:24.21.0-alpine3.24@sha256:ebfe2f90462722a7a4de65e91990e97fe0d401c70e0e762c5b53302f905ec1c1 AS console
ENV NPM_CONFIG_UPDATE_NOTIFIER=false
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN --mount=type=cache,target=/root/.npm npm ci --no-audit --no-fund
COPY web/ ./
# Typecheck and build into dist/, as make web-build does.
RUN npm run build

FROM --platform=$BUILDPLATFORM golang:1.27.2-alpine3.24@sha256:85dc1069ac644ea3c527b177303a406eb3358192816cd7f9e5848eb658851673 AS daemon
ARG TARGETOS
ARG TARGETARCH
ARG VERSION
# No cgo, as everywhere this binary is built; and the toolchain of this image,
# never one downloaded because go.mod asked for another.
ENV CGO_ENABLED=0 GOTOOLCHAIN=local
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY cmd/ cmd/
COPY internal/ internal/
# The flags of the Makefile's release build (backup-tool): -trimpath, no
# symbol table, the version stamped in.
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
      -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/mailserver ./cmd/mailserver
# The data directory, made here because the runtime image has no shell to make
# it: private to the daemon's user, as the daemon makes it elsewhere. A new
# named volume mounted on it takes this owner and mode.
RUN mkdir -p -m 0700 /out/data

FROM gcr.io/distroless/static-debian13:nonroot@sha256:e2e927ec666bae08560abb3c55d0659eceabb657f56b6782ab500a9fc7f555e3
ARG VERSION
LABEL org.opencontainers.image.title="Mailie" \
      org.opencontainers.image.description="REST and MCP over your mailboxes, with the server console" \
      org.opencontainers.image.source="https://github.com/thehappieco/mailie" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.version="${VERSION}"

COPY --from=daemon /out/mailserver /usr/local/bin/mailserver
# root's and read-only to the daemon, like the binary.
COPY --from=console /src/web/dist/ /usr/share/mailie/console/
COPY --from=daemon --chown=65532:65532 --chmod=0700 /out/data /data

# Listening on every address of the container is what makes the published port
# reach it; compose.yaml publishes that port on the host's loopback only, for a
# reverse proxy that terminates TLS. The three are the image's own paths:
# compose.yaml keeps them whatever its .env says.
ENV MAIL_DATA_DIR=/data \
    MAIL_HTTP_ADDR=0.0.0.0:8080 \
    MAIL_WEB_DIR=/usr/share/mailie/console

VOLUME /data
EXPOSE 8080
# distroless's nonroot, by number, so that a runtime can tell it is not root
# without reading /etc/passwd.
USER 65532:65532
# The daemon reads a .env from its working directory when there is one; at /
# there is none, and the configuration is the container's environment.
WORKDIR /
# No HEALTHCHECK: the image has no tool to run one. GET /v1/healthz from
# outside answers {"status":"ok",...} (docs/self-hosting.md, "Health").
# SIGTERM, Docker's default, makes the daemon close its connections and exit
# within 15 s; give a stop that long (compose.yaml allows 30 s).
ENTRYPOINT ["/usr/local/bin/mailserver"]
CMD ["serve"]
