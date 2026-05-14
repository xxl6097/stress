#!/usr/bin/env bash
# 多架构构建并推送到 CNB 私有仓库。
#
#   REGISTRY=docker.cnb.cool                   # 默认 docker.cnb.cool
#   REPO=abber/i/stress                        # 必填，CNB 上的仓库路径
#   IMAGE_TAG=v0.1.0                           # 默认 latest
#   PLATFORMS=linux/amd64,linux/arm64          # 默认两架构
#   PUSH=1                                     # 1=推送到 CNB，0=只在本地 buildx 缓存（默认 1）
#
# 用法：
#   ./scripts/build.sh
#   REPO=abber/i/stress IMAGE_TAG=v0.1.0 ./scripts/build.sh

set -euo pipefail

cd "$(dirname "$0")/.."

REGISTRY="${REGISTRY:-docker.cnb.cool}"
REPO="${REPO:-abber/i/stress}"
IMAGE_TAG="${IMAGE_TAG:-latest}"
PLATFORMS="${PLATFORMS:-linux/amd64,linux/arm64}"
PUSH="${PUSH:-1}"
REPO_NAME="${REPO_NAME:-stress}"
BUILDER="${BUILDER:-stress-builder}"

IMAGE="${REGISTRY}/${REPO}:${IMAGE_TAG}"

echo "==> 交叉编译 Go 二进制到 dist/"
mkdir -p dist
IFS=',' read -r -a _plats <<<"${PLATFORMS}"
for plat in "${_plats[@]}"; do
  os="${plat%%/*}"
  rest="${plat#*/}"
  arch="${rest%%/*}"
  variant=""
  if [[ "${rest}" == */* ]]; then
    variant="${rest#*/}"
  fi
  out="dist/${REPO_NAME}-${os}-${arch}"
  [[ -n "${variant}" ]] && out="${out}-${variant}"
  echo "    -> ${out}"
  CGO_ENABLED=0 GOOS="${os}" GOARCH="${arch}" GOARM="${variant#v}" \
    go build -trimpath -ldflags="-s -w" -o "${out}" .
done

echo "==> 准备 buildx builder：${BUILDER}"
if ! docker buildx inspect "${BUILDER}" >/dev/null 2>&1; then
  docker buildx create --name "${BUILDER}" --driver docker-container --use >/dev/null
else
  docker buildx use "${BUILDER}" >/dev/null
fi
docker buildx inspect --bootstrap "${BUILDER}" >/dev/null

PUSH_FLAG="--push"
[[ "${PUSH}" == "0" ]] && PUSH_FLAG="--load"
# 注意：--load 只支持单平台，多平台必须 --push
if [[ "${PUSH}" == "0" && "${PLATFORMS}" == *","* ]]; then
  echo "PUSH=0 时不能多平台，改成单平台或 PUSH=1" >&2
  exit 1
fi

echo "==> buildx build & ${PUSH_FLAG}：${IMAGE} (${PLATFORMS})"
docker buildx build \
  --builder "${BUILDER}" \
  --platform "${PLATFORMS}" \
  --build-arg "REPO_NAME=${REPO_NAME}" \
  --provenance=false \
  -t "${IMAGE}" \
  ${PUSH_FLAG} \
  .

echo "==> 完成：${IMAGE}"
