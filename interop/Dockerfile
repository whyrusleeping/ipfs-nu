FROM golang:1.26.0-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY p2p/ p2p/
COPY core/ core/
COPY httpsrc/ httpsrc/
COPY *.go ./
COPY interop/go.mod interop/go.sum interop/main.go interop/
WORKDIR /src/interop
RUN CGO_ENABLED=0 go build -trimpath -o /nu-interop .

FROM redis:7.4.2-bookworm
COPY --from=build /nu-interop /usr/local/bin/nu-interop
ENTRYPOINT ["nu-interop"]
CMD ["-mode", "transport"]
