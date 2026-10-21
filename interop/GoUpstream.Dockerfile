FROM golang:1.26.0-bookworm AS build
WORKDIR /src
COPY . .
WORKDIR /src/test-plans
RUN CGO_ENABLED=0 go build -mod=readonly -trimpath -o /testplan ./cmd/ping
FROM debian:bookworm-slim
COPY --from=build /testplan /testplan
ENTRYPOINT ["/testplan"]
