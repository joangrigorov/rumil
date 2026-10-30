# syntax=docker/dockerfile:1

# Build stage. The toolchain image is pinned to an immutable digest; the tag is
# kept alongside it for readability and for Dependabot to follow.
FROM golang:1.27.3-alpine3.24@sha256:d200d2dd76b03a99b52651fd1325aff7dcd35422c35c01bac56571b0507f3deb AS build

WORKDIR /src

# Dependency layer: changes to the source do not invalidate the module cache.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
RUN CGO_ENABLED=0 go build \
	-trimpath \
	-ldflags "-s -w -X main.version=${VERSION}" \
	-o /out/rumil ./cmd/rumil

# Runtime stage: distroless static, no shell, no package manager, running as the
# non-root user 65532.
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab

COPY --from=build /out/rumil /usr/local/bin/rumil

# An image is a distribution of the binary, so it carries the license text too.
COPY LICENSE /usr/share/licenses/rumil/LICENSE

USER 65532:65532
ENTRYPOINT ["/usr/local/bin/rumil"]

# `rumil review` arrived in Phase 1 and `rumil serve` in v0.0.3. The default
# command stays `--help`, so a bare `docker run` prints usage instead of failing
# on a missing config file.
CMD ["--help"]
