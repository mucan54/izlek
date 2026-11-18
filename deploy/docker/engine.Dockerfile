# Rust bileşenleri (shipper/compactor, query) için ortak imaj tarifi.
#   docker build -f deploy/docker/engine.Dockerfile --build-arg PKG=la-shipper --build-arg BIN=la-shipper -t la-shipper .
#   docker build -f deploy/docker/engine.Dockerfile --build-arg PKG=la-query   --build-arg BIN=la-query   -t la-query .
FROM rust:1.97-bookworm AS build
WORKDIR /src/engine
COPY engine/ ./
ARG PKG=la-shipper
ARG BIN=la-shipper
RUN --mount=type=cache,target=/usr/local/cargo/registry \
    --mount=type=cache,target=/src/engine/target \
    cargo build --release --locked -p ${PKG} --bin ${BIN} && cp target/release/${BIN} /out-bin

FROM gcr.io/distroless/cc-debian12:nonroot
COPY --from=build /out-bin /app
USER nonroot:nonroot
ENTRYPOINT ["/app"]
