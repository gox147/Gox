#!/usr/bin/env bash
# ============================================================================
# x11-dead-socket-probe.sh — 判定"本机 X server 是否会接住并挂死新连接"
# (T3-5 第二十批, 2026-11-20 凌晨批)
#
# 干什么: 对给定 DISPLAY 做 N 次"完整客户端"握手 (connect → Setup → 读满回复
# → 断开), 记录每一次的耗时与结果, 打印三档计数:
#   - 快   (< 2ms):  干净握手
#   - 慢   (>= 2ms): 握手被拖住 (X server 侧竞态)
#   - 失败          : 根本没拿到 Setup 回复
#
# 为什么要单独做一个探针: 冷启动脚本 (scripts/coldstart.sh) 每样本间 sleep
# 250ms 就是为了躲开这个竞态 (§12.12), 但那让**真实使用形态**(上一个 gox 刚
# 退出, 用户马上又起一个) 落在测量盲区里。§12.20 用它取证。
#
# 用法:
#   ./scripts/x11-dead-socket-probe.sh                 # 默认 --runs 20 --gap 0
#   ./scripts/x11-dead-socket-probe.sh --gap 1.2       # 样本间隔 (秒), 用来对照
#   ./scripts/x11-dead-socket-probe.sh --display :55
# 依赖: 只要 go (纯 net 包, 不引第三方), 无 DISPLAY 时直接跳过 (退出码 0)。
#
# 结果: stdout 一行汇总 + 逐样本明细; 末尾给出判定 (slow / clean / broken)。
# ============================================================================
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
RUNS=20
GAP=0
DISPLAY_ARG="${DISPLAY:-}"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --runs) RUNS="$2"; shift 2 ;;
    --gap) GAP="$2"; shift 2 ;;
    --display) DISPLAY_ARG="$2"; shift 2 ;;
    -h|--help) sed -n '2,30p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) echo "未知参数 $1 (见 --help)" >&2; exit 1 ;;
  esac
done

if [[ -z "$DISPLAY_ARG" ]]; then
  echo "[probe] 无 DISPLAY: 跳过 (不失败)" >&2
  exit 0
fi

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

cat > "$TMP/main.go" <<'GOEOF'
// 一次性探针: 完整客户端握手 N 次, 打印每次耗时。
package main

import (
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

// socketPath 按 xgb 的规则把 DISPLAY 换算成本机 unix socket 路径。
func socketPath(d string) string {
	if strings.HasPrefix(d, "/") {
		return d
	}
	num := strings.TrimPrefix(d, "unix:")
	if i := strings.LastIndex(num, ":"); i >= 0 {
		num = num[i+1:]
	}
	if i := strings.Index(num, "."); i >= 0 {
		num = num[:i]
	}
	return "/tmp/.X11-unix/X" + num
}

// handshake 做一次 "connect + Setup 请求 + 读满回复", 返回耗时与错误。
func handshake(sock string) (time.Duration, error) {
	start := time.Now()
	c, err := net.Dial("unix", sock)
	if err != nil {
		return time.Since(start), err
	}
	defer c.Close()
	// Setup: 小端, 协议 11.0, 无鉴权
	if _, err := c.Write([]byte{0x6c, 0, 11, 0, 0, 0, 0, 0, 0, 0, 0, 0}); err != nil {
		return time.Since(start), err
	}
	head := make([]byte, 8)
	if _, err := io.ReadFull(c, head); err != nil {
		return time.Since(start), err
	}
	body := make([]byte, (int(head[6])|int(head[7])<<8)*4)
	if _, err := io.ReadFull(c, body); err != nil {
		return time.Since(start), err
	}
	return time.Since(start), nil
}

func main() {
	sock := socketPath(os.Args[1])
	runs, _ := strconv.Atoi(os.Args[2])
	gap, _ := time.ParseDuration(os.Args[3])
	fast, slow, failed := 0, 0, 0
	for i := 0; i < runs; i++ {
		d, err := handshake(sock)
		switch {
		case err != nil:
			failed++
			fmt.Printf("  #%02d %-10v FAILED (%v)\n", i+1, d.Round(time.Microsecond), err)
		case d >= 2*time.Millisecond:
			slow++
			fmt.Printf("  #%02d %-10v slow\n", i+1, d.Round(time.Microsecond))
		default:
			fast++
			fmt.Printf("  #%02d %-10v ok\n", i+1, d.Round(time.Microsecond))
		}
		if gap > 0 && i != runs-1 {
			time.Sleep(gap)
		}
	}
	fmt.Printf("SUMMARY fast=%d slow=%d failed=%d runs=%d gap=%v socket=%s\n", fast, slow, failed, runs, gap, sock)
}
GOEOF

OUT="$TMP/out.txt"
(cd "$TMP" && GO111MODULE=off GOFLAGS= go run main.go "$DISPLAY_ARG" "$RUNS" "${GAP}s") >"$OUT" 2>&1
STATUS=$?
cat "$OUT"

# 判定: 有任何一次慢/失败就说明本机 X server 会挂住新连接。
# CI 里这行就是"环境指纹": 与 docs/performance.md §12.20 的数字对照用。
if grep -q '^SUMMARY' "$OUT"; then
  LINE="$(grep '^SUMMARY' "$OUT")"
  SLOW="$(sed -n 's/.*slow=\([0-9]*\).*/\1/p' <<<"$LINE")"
  FAILED="$(sed -n 's/.*failed=\([0-9]*\).*/\1/p' <<<"$LINE")"
  if [[ "${SLOW:-0}" -gt 0 || "${FAILED:-0}" -gt 0 ]]; then
    echo "[probe] 本机 X server 会挂住新连接 (slow=$SLOW failed=$FAILED): 此环境下冷启动数字会有双峰, 见 docs/performance.md §12.20"
  else
    echo "[probe] 本机 X server 握手干净 (slow=0 failed=0)"
  fi
fi
if [[ $STATUS -ne 0 ]]; then
  echo "[probe] 探针本身失败 (go run 退出码 $STATUS)" >&2
  exit 1
fi
exit 0
