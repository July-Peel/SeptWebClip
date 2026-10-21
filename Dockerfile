# syntax=docker/dockerfile:1

# ---- 编译 ----
FROM golang:1.26-alpine AS build
ARG GOPROXY=https://proxy.golang.org,direct
ENV CGO_ENABLED=0 GOPROXY=${GOPROXY}
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -trimpath -ldflags="-s -w" -o /out/septwebclip .

# ---- 运行：空白镜像 + 一个静态可执行文件 ----
FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/septwebclip /septwebclip
ENV DATA_DIR=/data \
    IMAGE_DIR=/data/images \
    LISTEN_ADDR=:8080
VOLUME ["/data"]
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 CMD ["/septwebclip", "-healthcheck"]
ENTRYPOINT ["/septwebclip"]
