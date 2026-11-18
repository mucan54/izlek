# Control plane + UI (statik SPA aynı imajdan sunulur).
#   docker build -f deploy/docker/control.Dockerfile -t la-control .
FROM node:22-bookworm-slim AS ui
WORKDIR /src
COPY sdk-web ./sdk-web
COPY ui ./ui
# UI, replay çözücüsünü sdk-web/src altından içe aktarır.
RUN cd ui && npm ci && npm run build

FROM golang:1.24-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY golib ./golib
COPY ingest ./ingest
COPY control ./control
COPY canary ./canary
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/app ./control/cmd/la-control

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/app /app
COPY --from=ui /src/ui/dist /ui
USER nonroot:nonroot
ENTRYPOINT ["/app"]
