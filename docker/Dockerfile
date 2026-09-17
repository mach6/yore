FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=0.1.0-dev
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags="-s -w -X github.com/mach6/yore/internal/cli.Version=${VERSION}" \
    -o /yore ./cmd/yore
# The runtime image has no shell to create this with. Docker seeds a new named
# volume from the image's directory, ownership included, so the nonroot server
# can write its database without anything claiming the volume first.
RUN mkdir /data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /yore /yore
COPY --from=build --chown=65532:65532 /data /data
VOLUME /data
EXPOSE 8080
ENTRYPOINT ["/yore", "server", "--db", "/data/yore.db", "--listen", ":8080"]
