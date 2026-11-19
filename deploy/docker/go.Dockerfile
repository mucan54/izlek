# Go bileşenleri (ingestion, control plane, canary) için ortak imaj tarifi.
#   docker build -f deploy/docker/go.Dockerfile --build-arg PKG=./ingest/cmd/la-ingest -t la-ingest .
#   docker build -f deploy/docker/go.Dockerfile --build-arg PKG=./control/cmd/la-control -t la-control .
#   docker build -f deploy/docker/go.Dockerfile --build-arg PKG=./canary/cmd/la-canary -t la-canary .
FROM golang:1.24-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY golib ./golib
COPY ingest ./ingest
COPY control ./control
COPY canary ./canary
ARG PKG=./ingest/cmd/la-ingest
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/app ${PKG}
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/la-healthcheck ./golib/cmd/la-healthcheck

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/app /app
# Health probe for compose/orchestrators: the image has no shell or curl.
COPY --from=build /out/la-healthcheck /la-healthcheck
USER nonroot:nonroot
ENTRYPOINT ["/app"]
