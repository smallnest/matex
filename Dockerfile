# syntax=docker/dockerfile:1
FROM golang:1.27.1-trixie AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /out/demo ./cmd/demo

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/demo /demo
COPY configs/config.yaml /configs/config.yaml
EXPOSE 8080
ENTRYPOINT ["/demo", "-conf", "/configs/config.yaml"]
