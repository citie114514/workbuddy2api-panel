#!/usr/bin/env bash
# 本地一键：编译当前代码 → 起一个网关实例（不需要 Docker / 不需要服务器）
#
#   ./scripts/run-local.sh                 # 本机平台编译，监听 127.0.0.1:7863
#   ./scripts/run-local.sh --port 7900     # 换端口
#   ./scripts/run-local.sh --api-key mykey # 换访问密钥（默认读 config，没有则 localtest）
#   ./scripts/run-local.sh --dir .run2     # 换运行目录（默认 .run，已被 .gitignore 忽略）
#
# 只编译 cmd/server（wb2api）。签到/改密/额度三个二进制本地调试用不到；
# 需要时用 scripts/build-image-prebuilt.sh --ctx-only 一次编全四个。
#
# 账号池默认是空的：/healthz 会显示 total=0，对话请求返回 503 no_healthy_account。
# 这是预期行为（本地没放 auths）。把账号 JSON 放进 <dir>/auths/ 即可真实对话。
set -euo pipefail
cd "$(dirname "$0")/.."

PORT=7863
API_KEY=""
RUN_DIR=".run"

while [ $# -gt 0 ]; do
  case "$1" in
    --port)    PORT="$2"; shift 2 ;;
    --api-key) API_KEY="$2"; shift 2 ;;
    --dir)     RUN_DIR="$2"; shift 2 ;;
    -h|--help) sed -n '2,20p' "$0" | grep '^#' | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) echo "未知参数: $1（--help 看用法）" >&2; exit 2 ;;
  esac
done

# --- 找 Go ---
GO_BIN="${GO_BIN:-}"
if [ -z "$GO_BIN" ]; then
  if command -v go >/dev/null 2>&1; then
    GO_BIN="$(command -v go)"
  elif [ -x "$HOME/.gotool/go/bin/go.exe" ]; then
    GO_BIN="$HOME/.gotool/go/bin/go.exe"
  elif [ -x "$HOME/.gotool/go/bin/go" ]; then
    GO_BIN="$HOME/.gotool/go/bin/go"
  else
    echo "找不到 Go。装 Go 1.22+ 或设 GO_BIN=/path/to/go" >&2
    exit 1
  fi
fi
echo "==> Go: $("$GO_BIN" version)"

# --- 目标平台：默认本机，交叉编译时用 GOOS/GOARCH 覆盖 ---
case "$(uname -s)" in
  MINGW*|MSYS*|CYGWIN*) HOST_OS=windows ;;
  Darwin)               HOST_OS=darwin ;;
  *)                    HOST_OS=linux ;;
esac
TARGET_OS="${GOOS:-$HOST_OS}"
if [ -n "${GOARCH:-}" ]; then
  TARGET_ARCH="$GOARCH"
else
  case "$(uname -m)" in
    x86_64|amd64)  TARGET_ARCH=amd64 ;;
    aarch64|arm64) TARGET_ARCH=arm64 ;;
    *)             TARGET_ARCH="$(uname -m)" ;;
  esac
fi
EXT=""; [ "$TARGET_OS" = windows ] && EXT=".exe"

mkdir -p "$RUN_DIR/auths"
BIN="$RUN_DIR/wb2api$EXT"

echo "==> 编译 → $BIN ($TARGET_OS/$TARGET_ARCH)"
CGO_ENABLED=0 GOOS="$TARGET_OS" GOARCH="$TARGET_ARCH" \
  "$GO_BIN" build -trimpath -ldflags "-s -w" -o "$BIN" ./cmd/server
ls -lh "$BIN" | awk '{print "    " $5, $9}'

# --- 生成运行配置（每次覆盖，避免旧端口残留）---
CFG="$RUN_DIR/config.json"
PY=""
for c in python3 python; do command -v "$c" >/dev/null 2>&1 && PY="$c" && break; done
if [ -z "$PY" ]; then
  echo "需要 python（生成 config.json）；或手动把 config.example.json 拷成 $CFG" >&2
  exit 1
fi
"$PY" - "$CFG" "$PORT" "$API_KEY" <<'PYEOF'
import json, os, sys
cfg_path, port, api_key = sys.argv[1], int(sys.argv[2]), sys.argv[3]
cfg = json.load(open('config.example.json', encoding='utf-8'))
run_dir = os.path.dirname(os.path.abspath(cfg_path)).replace('\\', '/')
cfg['listen'] = '127.0.0.1:%d' % port
cfg['auth_dir'] = run_dir + '/auths'
cfg['state_file'] = run_dir + '/data/state.json'
os.makedirs(run_dir + '/data', exist_ok=True)
if api_key:
    cfg['api_key'] = api_key
elif not cfg.get('api_key'):
    cfg['api_key'] = 'localtest'
json.dump(cfg, open(cfg_path, 'w', encoding='utf-8'), ensure_ascii=False, indent=2)
print('    api_key =', cfg['api_key'])
PYEOF

echo
echo "==> 启动：http://127.0.0.1:$PORT"
echo "    面板    http://127.0.0.1:$PORT/panel/"
echo "    健康    curl -s http://127.0.0.1:$PORT/healthz"
echo "    配置    $CFG"
echo "    停止    Ctrl+C"
echo
exec "$BIN" -config "$CFG"
