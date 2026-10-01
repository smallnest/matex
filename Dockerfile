# syntax=docker/dockerfile:1
FROM golang:1.27.1-trixie AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# 纯 Go 构建：modernc.org/sqlite 不需要 cgo，产出的静态二进制可直接跑在 distroless。
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /out/demo ./cmd/demo

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/demo /demo
# 默认装的是零依赖配置（http + log + 服务段），镜像开箱即起。
# 接真实依赖（db / redis / memcache / kafka）时，挂载一份完整配置覆盖它：
#   docker run -p 8080:8080 -v "$PWD/configs/config.yaml:/configs/config.yaml:ro" matex:latest
# 说明：「配了才建」——只有配置里出现了对应段才会初始化，所以不能只靠环境变量开启 db/redis，
# 必须让 /configs/config.yaml 里存在那些段。
COPY configs/config.min.yaml /configs/config.yaml
EXPOSE 8080
ENTRYPOINT ["/demo", "-conf", "/configs/config.yaml"]
