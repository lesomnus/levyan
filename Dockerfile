# levyan as a deployment runs it: `levyan` and `levyan-admin`, on an image with
# nothing else in it.
#
#   docker build -t levyan --build-arg VERSION=$(git describe --tags --always) .
#
# Everything it is told comes from the environment (`LEVYAN_*`) or a file named
# with `--config`; there is no `levyan.yaml` in it, so a checkout's development
# defaults never reach a deployment.

# The binaries, static: the SQLite driver is WebAssembly in Go and asks for no
# C library, so neither does anything else. The toolchain stamps the commit
# from the repository, and VERSION names the release (`levyan version`).
#
# The patch release, named: the standard library is linked into the binary,
# so its fixes arrive only by building with the release that has them.
FROM golang:1.27.3-bookworm AS go
ARG VERSION=
WORKDIR /src
# The API is a module of its own that this one replaces with its directory
# (go.mod), so its go.mod is wanted before anything is downloaded.
COPY go.mod go.sum ./
COPY api/go.mod api/go.sum ./api/
RUN go mod download
COPY . .
RUN --mount=type=cache,id=levyan-go-build,target=/root/.cache/go-build \
	flags="-s -w -X github.com/lesomnus/payday/version.version=${VERSION}" \
	&& CGO_ENABLED=0 go build -trimpath -ldflags="${flags}" -o /out/levyan ./cmd/levyan \
	&& CGO_ENABLED=0 go build -trimpath -ldflags="${flags}" -o /out/levyan-admin ./cmd/levyan-admin
# Where a deployment's state goes -- a SQLite file while trying it, the trail's
# archive -- made here so that a volume mounted on it starts out owned by the
# user below.
RUN mkdir -p /out/state

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=go /out/levyan /out/levyan-admin /usr/local/bin/
COPY --from=go --chown=nonroot:nonroot /out/state /var/lib/levyan
WORKDIR /var/lib/levyan
USER nonroot:nonroot
# 50051 is what products call (`levyan serve`), and 8081 the operator's entry
# point (`levyan-admin serve`), which is never published to the public.
EXPOSE 50051 8081
ENTRYPOINT ["/usr/local/bin/levyan"]
CMD ["serve"]
