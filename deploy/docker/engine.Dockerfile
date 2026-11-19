# syntax=docker/dockerfile:1
# Rust bileşenleri: tek derleme aşaması, iki hedef.
#   docker build -f deploy/docker/engine.Dockerfile --target shipper -t la-shipper .   # ship | compact | rebuild
#   docker build -f deploy/docker/engine.Dockerfile --target query   -t la-query .
#
# Derleme belleği: DataFusion'ın release derlemesi birkaç GB RAM ister. Küçük makinede
# --build-arg CARGO_BUILD_JOBS=2 ile paralelliği düşürün (daha yavaş, daha az bellek).
FROM rust:1.97-bookworm AS build
WORKDIR /src/engine
COPY engine/ ./
ARG CARGO_BUILD_JOBS
# İmajda hata ayıklama bilgisi taşınmaz (daha küçük ikili, daha az derleme belleği).
ENV CARGO_PROFILE_RELEASE_DEBUG=0
RUN --mount=type=cache,target=/usr/local/cargo/registry \
    --mount=type=cache,target=/src/engine/target \
    if [ -z "$CARGO_BUILD_JOBS" ]; then unset CARGO_BUILD_JOBS; fi; \
    cargo build --release --locked -p la-shipper -p la-query \
    && mkdir /out && cp target/release/la-shipper target/release/la-query /out/

# Sağlık yoklayıcısı (imajlarda kabuk ve curl yok). Yalnızca standart kütüphane kullanır.
FROM golang:1.24-bookworm AS health
WORKDIR /src
COPY go.mod go.sum ./
COPY golib/cmd/la-healthcheck ./golib/cmd/la-healthcheck
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/la-healthcheck ./golib/cmd/la-healthcheck

FROM gcr.io/distroless/cc-debian12:nonroot AS shipper
COPY --from=build /out/la-shipper /app
COPY --from=health /out/la-healthcheck /la-healthcheck
USER nonroot:nonroot
ENTRYPOINT ["/app"]

FROM gcr.io/distroless/cc-debian12:nonroot AS query
COPY --from=build /out/la-query /app
COPY --from=health /out/la-healthcheck /la-healthcheck
USER nonroot:nonroot
ENTRYPOINT ["/app"]
