# syntax=docker/dockerfile:1
# Rust bileşenleri: tek derleme aşaması, iki hedef.
#   docker build -f deploy/docker/engine.Dockerfile --target shipper -t la-shipper .   # ship | compact | rebuild
#   docker build -f deploy/docker/engine.Dockerfile --target query   -t la-query .
FROM rust:1.97-bookworm AS build
WORKDIR /src/engine
COPY engine/ ./
RUN --mount=type=cache,target=/usr/local/cargo/registry \
    --mount=type=cache,target=/src/engine/target \
    cargo build --release --locked -p la-shipper -p la-query \
    && mkdir /out && cp target/release/la-shipper target/release/la-query /out/

FROM gcr.io/distroless/cc-debian12:nonroot AS shipper
COPY --from=build /out/la-shipper /app
USER nonroot:nonroot
ENTRYPOINT ["/app"]

FROM gcr.io/distroless/cc-debian12:nonroot AS query
COPY --from=build /out/la-query /app
USER nonroot:nonroot
ENTRYPOINT ["/app"]
