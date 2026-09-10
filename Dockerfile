FROM golang:1.26-alpine AS build
WORKDIR /src
ENV GOTOOLCHAIN=local
# 可覆盖依赖下载代理；保留 Go 校验和验证。
ARG GOPROXY=https://goproxy.cn,direct
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -o /server ./cmd/server

FROM alpine:3.22
RUN apk add --no-cache ca-certificates && addgroup -g 10001 recorder && adduser -D -u 10001 -G recorder recorder \
    && mkdir -p /app/uploads && chown -R recorder:recorder /app
WORKDIR /app
COPY --from=build /server ./server
USER recorder
EXPOSE 8080
ENTRYPOINT ["/app/server"]
