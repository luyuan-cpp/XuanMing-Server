# syntax=docker/dockerfile:1.7

# Pandora Go 业务服务通用镜像(15 个服务共用一份 Dockerfile)。
#
# 本仓库是 go.work 多模块工作区(根目录没有 go.mod),所以构建上下文必须是仓库根,
# 把整个 workspace 拷进来,再按 SERVICE_DIR / CMD_NAME 构建指定服务的 cmd。
#
# 构建(由 tools/scripts/start.ps1 / docker-compose.services.yml 自动带 build-arg):
#   docker build -f deploy/services/Dockerfile \
#     --build-arg SERVICE_DIR=services/account/login \
#     --build-arg CMD_NAME=login \
#     -t pandora/login:dev .
#
# 运行期配置:镜像内已烘焙各服务 etc/(含 killswitch.yaml 兜底),但 127.0.0.1 地址
# 在容器里不可用 —— compose 用 bind-mount、k8s 用 ConfigMap 覆盖主配置为「集群版」
# (mysql/redis/kafka/etcd 改容器服务名,见 tools/scripts/gen_cluster_config.ps1)。

ARG GO_VERSION=1.26.4

# ===== 构建阶段 =====
FROM golang:${GO_VERSION} AS builder

ARG SERVICE_DIR
ARG CMD_NAME

# 版本烙印:由 start.ps1 / compose 传入 git 版本信息,编译期 -ldflags -X 注入二进制。
# 不传时用占位默认值(本地随手 build 也能跑),线上发布务必传真实 git tag/commit。
ARG VERSION=dev
ARG GIT_COMMIT=unknown
ARG BUILD_TIME=unknown

WORKDIR /src

# 拷整个 workspace(go.work + pkg + proto + services)。.dockerignore 已排除产物/缓存。
COPY . .

# 纯 Go 依赖(mysql/redis/sarama 均无 CGO),静态编译产出单文件二进制。
ENV CGO_ENABLED=0 GOOS=linux GOFLAGS=-trimpath
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    test -n "$SERVICE_DIR" || (echo "ERROR: build-arg SERVICE_DIR 必填" && exit 1) \
    && test -n "$CMD_NAME" || (echo "ERROR: build-arg CMD_NAME 必填" && exit 1) \
    && cd "/src/${SERVICE_DIR}" \
    && go build \
        -ldflags="-s -w \
            -X github.com/luyuancpp/pandora/pkg/version.Version=${VERSION} \
            -X github.com/luyuancpp/pandora/pkg/version.Commit=${GIT_COMMIT} \
            -X github.com/luyuancpp/pandora/pkg/version.BuildTime=${BUILD_TIME}" \
        -o /out/app "./cmd/${CMD_NAME}"

# ===== 运行阶段 =====
FROM alpine:3.20

ARG SERVICE_DIR

RUN apk add --no-cache ca-certificates tzdata wget \
    && adduser -D -u 10001 pandora
WORKDIR /app

COPY --from=builder /out/app /app/app
# 烘焙该服务的 etc/(killswitch.yaml 等兜底文件);主配置运行期会被集群版覆盖。
COPY --chown=pandora:pandora ${SERVICE_DIR}/etc/ /app/etc/

USER pandora

# 约定:集群版主配置统一挂成 /app/etc/cluster.yaml(compose bind-mount / k8s ConfigMap)。
# compose、k8s 也可显式覆盖 -conf;独立 docker run 时默认读 cluster.yaml。
ENTRYPOINT ["/app/app"]
CMD ["-conf", "etc/cluster.yaml"]
