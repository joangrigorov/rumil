# syntax=docker/dockerfile:1

# Build stage. The toolchain image is pinned to an immutable digest; the tag is
# kept alongside it for readability and for Dependabot to follow.
FROM golang:1.27.2-alpine3.24@sha256:f92b6ef800e499660581efdabdf25d9d817a9d124eaf900924f0504e7e27e12d AS build

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

USER 65532:65532
ENTRYPOINT ["/usr/local/bin/rumil"]

# `rumil review` arrived in Phase 1 and `rumil serve` in v0.0.3. The default
# command stays `--help`, so a bare `docker run` prints usage instead of failing
# on a missing config file.
CMD ["--help"]
