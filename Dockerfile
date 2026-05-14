# syntax=docker/dockerfile:1.7

#FROM alpine AS runner
FROM docker.cnb.cool/clife/golang/dokcer/alpine:latest AS runner
# 设置工作空间
WORKDIR /app
# 声明构建参数（由buildx自动注入）
ARG TARGETOS
ARG TARGETARCH
ARG TARGETVARIANT
# 服务名称（可在 buildx --build-arg REPO_NAME=xxx 覆盖）
ARG REPO_NAME=stress
COPY entrypoint.sh ./
RUN chmod +x ./entrypoint.sh
# 更具不同操作系统，不同CPU架构复制到镜像
COPY dist/${REPO_NAME}-${TARGETOS}-${TARGETARCH}${TARGETVARIANT:+-${TARGETVARIANT}} ./main
ENV PUID=0 PGID=0 UMASK=022
# 设置入口点
CMD ["./entrypoint.sh"]
