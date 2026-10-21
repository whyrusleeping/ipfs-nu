FROM rust:1.90.0-bookworm AS build
RUN apt-get update && apt-get install -y --no-install-recommends protobuf-compiler && rm -rf /var/lib/apt/lists/*
WORKDIR /src
COPY . .
ENV CARGO_BUILD_JOBS=4
RUN cargo build --locked --release --package interop-tests --bin native_ping
FROM debian:bookworm-slim
COPY --from=build /src/target/release/native_ping /testplan
ENTRYPOINT ["/testplan"]
