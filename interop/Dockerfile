FROM golang:1.26.0-bookworm AS build
WORKDIR /src
# Dockerfile.dockerignore restricts this to Go sources/modules and API embeds.
# Include the daemon assembly as well as the original minimal-host driver.
COPY . .
WORKDIR /src/interop
RUN CGO_ENABLED=0 go build -trimpath -o /nu-interop .

FROM redis:7.4.2-bookworm
COPY --from=build /nu-interop /usr/local/bin/nu-interop
ENTRYPOINT ["nu-interop"]
CMD ["-mode", "transport"]
