FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /yore ./cmd/yore

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /yore /yore
VOLUME /data
EXPOSE 8080
ENTRYPOINT ["/yore", "server", "--db", "/data/yore.db", "--listen", ":8080"]
