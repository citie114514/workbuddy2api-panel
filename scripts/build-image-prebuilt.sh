#!/usr/bin/env bash
# 本地交叉编译 → 组装最小构建上下文 → docker build。
#
# 适用：1) 构建机没装 Docker（做不了 docker build），但装了 Go；
#      2) 目标机内存有限，docker build 里跑 go build 会 OOM。
# 编译在调用方完成（需要 Go ≥ 1.22），容器里只做 COPY，构建峰值内存 < 100M。
#
#   ./scripts/build-image-prebuilt.sh                      本地编译 + 直接 docker build
#   ./scripts/build-image-prebuilt.sh --ctx-only           本地编译 + 只组装上下文（不打镜像）
#   ./scripts/build-image-prebuilt.sh --save               本地编译 + build + 导出 tar.gz
#   ./scripts/build-image-prebuilt.sh my/tag:1.0 --save    自定义 tag 并导出
#   GOOS=linux GOARCH=arm64 ./scripts/build-image-prebuilt.sh     交叉到 arm64（CGO 已关）
#
# --ctx-only 的典型用法（本机没 Docker，或要在小内存机器上构建）：
#   ./scripts/build-image-prebuilt.sh --ctx-only
#   scp -r prebuilt-ctx root@host:/root/
#   ssh root@host 'cd /root/prebuilt-ctx && docker build -t workbuddy2api-panel:1.14.3-integrated .'
#
# --save 的典型用法（拿不到镜像仓库、目标机不能联网拉基础镜像时）：
#   ./scripts/build-image-prebuilt.sh --save
#   scp bin/workbuddy2api_panel_1.14.3-integrated.tar.gz root@host:/root/
#   ssh root@host 'gunzip -c workbuddy2api_panel_1.14.3-integrated.tar.gz | docker load'
set -euo pipefail

TAG="workbuddy2api-panel:1.14.3-integrated"
SAVE=0
CTX_ONLY=0
for arg in "$@"; do
  case "$arg" in
    --save)     SAVE=1 ;;
    --ctx-only) CTX_ONLY=1 ;;
    -*)         echo "未知参数: $arg" >&2; exit 2 ;;
    *)          TAG="$arg" ;;
  esac
done

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
export CGO_ENABLED=0 GOOS="${GOOS:-linux}" GOARCH="${GOARCH:-amd64}"
OUT="${ROOT}/bin/${GOOS}-${GOARCH}"

echo "==> 交叉编译 (GOOS=${GOOS} GOARCH=${GOARCH})"
mkdir -p "$OUT"
# 四个二进制：主服务 + 容器内可调用的签到/登录/积分工具（与主 Dockerfile 一致）
build() { go build -trimpath -ldflags="-s -w" -o "$OUT/$2" "./cmd/$1"; echo "    $2  $(du -h "$OUT/$2" | cut -f1)"; }
build server wb2api
build signin signin_bin
build login  login
build credit credit

echo "==> 组装构建上下文"
if [[ $CTX_ONLY -eq 1 ]]; then
  CTX="${ROOT}/prebuilt-ctx"
  rm -rf "$CTX"
else
  CTX="$(mktemp -d)"
  trap 'rm -rf "$CTX"' EXIT
fi
mkdir -p "$CTX/bin" "$CTX/scripts"
cp "$OUT/wb2api" "$OUT/signin_bin" "$OUT/login" "$OUT/credit" "$CTX/bin/"
cp "$ROOT/login.sh" "$ROOT/signin.sh" "$ROOT/credit.sh" "$CTX/"
cp "$ROOT/scripts/probe_active.py" "$CTX/scripts/"
cp "$ROOT/config.example.json" "$CTX/"
cp "$ROOT/Dockerfile.prebuilt" "$CTX/Dockerfile"

if [[ $CTX_ONLY -eq 1 ]]; then
  echo "==> 上下文已就绪: $CTX （$(du -sh "$CTX" | cut -f1)）"
  echo "    上传并在目标机构建:"
  echo "      scp -r $CTX root@<host>:/root/"
  echo "      ssh root@<host> 'cd /root/$(basename "$CTX") && docker build -t $TAG .'"
  exit 0
fi

if ! command -v docker >/dev/null 2>&1; then
  echo "!! 本机没有 docker，改用 --ctx-only 后到目标机上构建" >&2
  exit 1
fi

echo "==> docker build -t $TAG"
docker build -t "$TAG" "$CTX"

if [[ $SAVE -eq 1 ]]; then
  SAFE="${TAG//[:\/]/_}"
  mkdir -p "$ROOT/bin"
  echo "==> 导出 tar.gz（41MB 级镜像压缩后约 15-20MB）"
  docker save "$TAG" | gzip -1 > "$ROOT/bin/${SAFE}.tar.gz"
  echo "    已导出 $ROOT/bin/${SAFE}.tar.gz（$(du -h "$ROOT/bin/${SAFE}.tar.gz" | cut -f1)）"
  echo "    落地: gunzip -c ${SAFE}.tar.gz | docker load"
fi

echo "==> 完成: $TAG"
echo "    验证二进制可执行: docker run --rm --entrypoint /bin/sh $TAG -c '/app/wb2api -h 2>&1 | head -3'"
