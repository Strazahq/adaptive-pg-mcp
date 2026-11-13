# The adaptive-pg-mcp image: one static binary on distroless static, run as
# the nonroot user. Both base images are pinned by digest. Change a tag and
# its digest together.

# The build stage runs on the platform of the builder and cross-compiles for
# the target, so a multi-platform build needs no emulation.
FROM --platform=$BUILDPLATFORM golang:1.27.4-alpine3.24@sha256:0a5f7fe796599ff487605987432e7619ae45beeb2fba4c2ce0b907879932aebf AS build
ARG TARGETOS
ARG TARGETARCH
# VERSION is what the server reports as its version. Empty leaves (devel).
ARG VERSION=
# GOTOOLCHAIN=local fails the build when go.mod pins a newer toolchain than
# this image, instead of downloading one.
ENV CGO_ENABLED=0 GOTOOLCHAIN=local
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
      -ldflags "-X github.com/strazahq/adaptive-pg-mcp/internal/core.version=${VERSION}" \
      -o /out/adaptive-pg-mcp ./cmd/adaptive-pg-mcp

FROM gcr.io/distroless/static-debian13:nonroot@sha256:18f4cc26c9ffa7f20a359298be7deadac5c607c8e03a02b8119f1a5fc2f82c99
LABEL org.opencontainers.image.title="adaptive-pg-mcp" \
      org.opencontainers.image.description="MCP server for PostgreSQL with typed tools, catalog SQL as named tools and free SQL under the grants of each login" \
      org.opencontainers.image.source="https://github.com/strazahq/adaptive-pg-mcp" \
      org.opencontainers.image.licenses="Apache-2.0"
COPY LICENSE NOTICE /usr/share/doc/adaptive-pg-mcp/
COPY --from=build /out/adaptive-pg-mcp /usr/local/bin/adaptive-pg-mcp
# 65532 is the nonroot user of distroless. It is given by number, because
# Kubernetes cannot check runAsNonRoot against a user name.
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/adaptive-pg-mcp"]
CMD ["serve", "-config", "/etc/adaptive-pg-mcp/server.yaml"]
