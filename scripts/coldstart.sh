#!/usr/bin/env bash
# ============================================================================
# coldstart.sh — 进程级"冷启动到首帧"基准 (T2-5, 2026-10-28)
#
# 口径: **进程级** —— 外部 wall time, 从进程 exec 到脚本打印 FIRST_FRAME:
#   进程 exec + 动态链接 + Go runtime 启动
#   + 解释器初始化 + 脚本求值 (含 ES 模块装载)
#   + 真实窗口创建 (X11 等) + 首次 Layout + 首次 Draw + **真实上屏**
#   + JS 侧 1ms 轮询探测到首帧 + 打印标记 + 关窗退出 (≤2ms 开销)
#
# 与 tools/perf 的 coldstart-*.json (进程内口径) **不可混用**: 那边不含
# 进程 exec / Go runtime 启动, 用的是无窗口替身 Surface; 这边是真窗口。
# 对外宣传数字用本脚本的结果 (用户双击 exe 的体感), 口径说明见
# docs/performance.md §12。
#
# 跑法:
#   ./scripts/coldstart.sh                # 默认 7 次取中位数
#   ./scripts/coldstart.sh --runs 15      # 加密重复
#   ./scripts/coldstart.sh --interval 0.3 # 样本间隔秒数 (默认 0.25, 见下)
#   ./scripts/coldstart.sh --xvfb         # 无 DISPLAY 时强制用 Xvfb 虚拟显示
#
# --- 样本间隔 (T3-5 第十二批, 2026-11-19) ---
#
# 旧版背靠背连跑, 每个样本都被"上一个 gox 刚断开 X 连接"的服务器侧清理
# 推迟 ~15ms: 实测同一个纯 xgb 连接程序 (只 dial + 握手, 不建窗口) 连跑三次
# 是 0.25ms / 19.3ms / 19.2ms, 间隔 0.3s 后恢复到 0.2ms 量级。也就是说旧口径
# 测到的"38ms"里约 14ms 是 Xvfb 的 accept 延迟伪影, 不是 gox 的成本。
# 现在样本之间默认等 0.25s, 并把 interval_ms 写进结果 JSON。
# **这是测量口径修正, 不是性能优化**: 新旧数字不可相减, 引用时必须带口径。
#
# 显示环境: 有 DISPLAY 直接用; 没有 xvfb-run 时跳过 (不失败, 与
# bench-compare.sh 的"缺栈跳过"纪律一致)。
#
# 结果: <out>/coldstart-process-<UTC时间>.json + stdout 一张表。
# ============================================================================
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
RUNS=7
OUT="$ROOT/bench-results"
WANT_XVFB=0
# 样本间隔 (秒)。0 表示旧口径 (背靠背), 仅供复现历史数字。
INTERVAL=0.25

GREP=/usr/bin/grep
[[ -x "$GREP" ]] || GREP=$(command -v grep)

while [[ $# -gt 0 ]]; do
  case "$1" in
    --runs) RUNS="$2"; shift 2 ;;
    --out)  OUT="$2"; shift 2 ;;
    --interval) INTERVAL="$2"; shift 2 ;;
    --xvfb) WANT_XVFB=1; shift ;;
    -h|--help) "$GREP" '^#' "$0" | sed 's/^# \{0,1\}//' | head -40; exit 0 ;;
    *) echo "未知参数 $1 (见 --help)"; exit 1 ;;
  esac
done
mkdir -p "$OUT"

# -------- 显示环境 --------
#
# 冷启动到首帧必须真的上屏。有 DISPLAY 就用; 没有/被要求 --xvfb 时用
# Xvfb 虚拟显示 (软件渲染, 无合成器, 数字稳定可复现)。两者都没有就跳过。
RUNNER=()
XVFB_PID=""
DISPLAY_MODE="existing"
cleanup() { [[ -n "$XVFB_PID" ]] && kill "$XVFB_PID" 2>/dev/null; XVFB_PID=""; }
trap cleanup EXIT INT TERM

if [[ -n "${DISPLAY:-}" && "$WANT_XVFB" -eq 0 ]]; then
  echo "[coldstart] 使用现有显示: DISPLAY=$DISPLAY" >&2
elif command -v Xvfb >/dev/null 2>&1; then
  # 本脚本自己起**一个** Xvfb 并全程复用 (第十七批, 2026-11-19)。
  #
  # 为什么不再用 `xvfb-run -a` 包每个样本: 那会在每个样本里新起一个 X server,
  # 把一个 30ms 级的"Xvfb 启动 + 监听 socket"算进了 gox 的首帧里。同机实测:
  # 每样本 xvfb-run = 41ms, 复用预热好的 :55 = 9ms —— 前者不是 gox 的成本。
  # 这与"样本间隔"是同一类口径问题: 测的是运行时的开销, 不是测试支架的开销。
  DISPLAY_MODE="xvfb-managed"
  for d in $(seq 90 120); do
    if [[ ! -e "/tmp/.X11-unix/X$d" ]]; then
      Xvfb ":$d" -screen 0 1024x768x24 >/dev/null 2>&1 &
      XVFB_PID=$!
      export DISPLAY=":$d"
      for _ in $(seq 1 100); do [[ -e "/tmp/.X11-unix/X$d" ]] && break; sleep 0.02; done
      break
    fi
  done
  [[ -n "$XVFB_PID" ]] || { echo "[coldstart] 起 Xvfb 失败 (无空闲 display)" >&2; exit 1; }
  echo "[coldstart] 用自管 Xvfb ($DISPLAY, 1024x768x24, 全程复用一次)" >&2
else
  echo "[coldstart] 无 DISPLAY 且无 xvfb-run: 进程级首帧口径跳过 (不失败)。" >&2
  echo "[coldstart] 进程内口径仍可用: go test ./tools/perf/ -run TestColdStartToFirstFrame" >&2
  exit 0
fi

# -------- 构建 --------
echo "[coldstart] 构建本仓库 gox ..." >&2
GOX_BIN="$OUT/gox-coldstart"
(cd "$ROOT" && go build -o "$GOX_BIN" ./cmd/gox) >&2

SCRIPT="$ROOT/scripts/bench/coldstart_gui.js"
[[ -f "$SCRIPT" ]] || { echo "[coldstart] 找不到 $SCRIPT" >&2; exit 1; }

# -------- 计量 --------
#
# 时钟用 bash 内建 $EPOCHREALTIME (微秒), **不再每个样本 fork 两次 `date`**。
# 第十七批实测: 两次 `date +%s%N` 的 fork/exec 落在 t1 那一侧, 会把 ~0.9ms
# 的支架开销记到 gox 头上 (同机同二进制: fork 版 9ms / 内建时钟版 8.1ms)。
# 只有 bash < 5 或变量被清掉时才退回 date。
now_us() {
  if [[ -n "${EPOCHREALTIME:-}" ]]; then
    local t=${EPOCHREALTIME/[.,]/}
    printf '%s' "$((t))"
  else
    date +%s%6N
  fi
}

run_once_ms() {
  local t0 t1 out
  t0=$(now_us)
  if ! out=$("${RUNNER[@]}" "$GOX_BIN" "$SCRIPT" 2>/dev/null); then
    echo "[coldstart] 运行失败 (见下方 stderr):" >&2
    "${RUNNER[@]}" "$GOX_BIN" "$SCRIPT" >&2 || true
    exit 1
  fi
  t1=$(now_us)
  # 首帧标记必须真的打出来: 没打出来说明探测循环没等到上屏, 数字无效。
  if [[ "$out" != *"FIRST_FRAME"* ]]; then
    echo "[coldstart] 未检测到 FIRST_FRAME 标记 (输出: $out)" >&2
    exit 1
  fi
  # 四舍五入到毫秒 (旧实现是截断, 与 date 的整毫秒相除一致取值偏低)
  echo $(( (t1 - t0 + 500) / 1000 ))
}

run_rss_kb() {
  local out
  if [[ -x /usr/bin/time ]] && /usr/bin/time -v true >/dev/null 2>&1; then
    out=$(/usr/bin/time -v "${RUNNER[@]}" "$GOX_BIN" "$SCRIPT" 2>&1 >/dev/null | "$GREP" "Maximum resident" | "$GREP" -oE '[0-9]+' | tail -1)
    [[ -n "$out" ]] && { echo "$out"; return; }
  fi
  echo 0
}

median() { printf '%s\n' "$@" | sort -n | awk '{a[NR]=$1} END {print (NR%2) ? a[(NR+1)/2] : int((a[NR/2]+a[NR/2+1])/2)}'; }

# 预热 2 次: 二进制页缓存 / 字体栅格化缓存 / X 连接建立, 消除首个进程的冷启动偏置。
for _ in 1 2; do "${RUNNER[@]}" "$GOX_BIN" "$SCRIPT" >/dev/null 2>&1 || true; sleep "$INTERVAL"; done

SAMPLES=()
for i in $(seq 1 "$RUNS"); do
  ms=$(run_once_ms)
  SAMPLES+=("$ms")
  echo "[coldstart] 第 $i/$RUNS 次: ${ms}ms" >&2
  # 间隔见文件头 "样本间隔": 不等的话每个样本都会被 X server 的断开清理推迟。
  sleep "$INTERVAL"
done
MED=$(median "${SAMPLES[@]}")
RSS=$(run_rss_kb)

# -------- JSON + 表 --------
STAMP=$(date -u +%Y%m%d-%H%M%S)
JSON="$OUT/coldstart-process-$STAMP.json"
GOX_COLDSTART_CLOCK=$([[ -n "${EPOCHREALTIME:-}" ]] && echo "bash-epochrealtime(us, no fork)" || echo "date(us, 2 forks/sample)")
GOX_COLDSTART_DISPLAY="$DISPLAY_MODE"
export GOX_COLDSTART_CLOCK GOX_COLDSTART_DISPLAY
python3 - "$JSON" "$MED" "$RSS" "$RUNS" "${SAMPLES[*]}" "$INTERVAL" <<'PYEOF'
import sys, json, datetime, os, platform, subprocess

json_path, med, rss, runs, samples = sys.argv[1], int(sys.argv[2]), int(sys.argv[3]), int(sys.argv[4]), [int(x) for x in sys.argv[5].split()]
interval_ms = int(round(float(sys.argv[6]) * 1000))

def sh(cmd):
    try:
        return subprocess.check_output(cmd, shell=True, text=True, stderr=subprocess.DEVNULL).strip()
    except Exception:
        return ""

cpu = sh("sysctl -n machdep.cpu.brand_string")
if not cpu:
    try:
        with open("/proc/cpuinfo") as f:
            for ln in f:
                if ln.startswith("model name"):
                    cpu = ln.split(":", 1)[1].strip(); break
    except Exception:
        pass

doc = {
    "generated_at": datetime.datetime.now(datetime.timezone.utc).isoformat(),
    "benchmark": "coldstart-process",
    "runs": runs,
    "first_frame_ms_median": med,
    "first_frame_ms_all": samples,
    "rss_kb": rss,
    "interval_ms": interval_ms,
    "harness": {
        "clock": os.environ.get("GOX_COLDSTART_CLOCK", "unknown"),
        "display_mode": os.environ.get("GOX_COLDSTART_DISPLAY", "unknown"),
        "coldstart_sh_rev": "T3-5-17 (2026-11-19): 内建时钟 + 单 Xvfb 复用",
        "note": "2026-11-19 第十七批之前的样本含 ~0.9ms `date` fork 与(在 --xvfb 模式下)~30ms 的 Xvfb 启动成本, 与本节之前的历史数字不可直接相减; 见 docs/performance.md §12.17",
    },
    "method": "外部 wall time (进程 exec → 脚本打印 FIRST_FRAME → 进程退出, 含 ≤2ms 探测与 ~0.7ms 退出清理, 见 §12.17), 跑 %d 次取中位数, 预热 2 次不计入, 样本间隔 %dms (避开 X server 断开清理造成的 accept 延迟, 见脚本头); 首帧信号 = JS 侧 1ms 轮询 gx/dev 的 devSnapshot().frame.count (只在真实上屏出口计数)" % (runs, interval_ms),
    "scope": "进程级冷启动到首帧: 含进程 exec/动态链接/Go runtime 启动/解释器初始化/脚本求值(含模块装载)/真实窗口创建/首次 Layout+Draw+上屏",
    "host": {
        "platform": platform.platform(),
        "machine": platform.machine(),
        "cpu": cpu,
        "cores": os.cpu_count(),
        "display": os.environ.get("DISPLAY", ""),
        "go": sh("go version") or "go (构建本二进制的工具链)",
    },
    "note": "对外可引用的'单文件分发冷启动到首帧'口径; 与 tools/perf 的进程内口径 (coldstart-*.json) 不可混用",
}
json.dump(doc, open(json_path, "w"), ensure_ascii=False, indent=2)
print(json.dumps(doc, ensure_ascii=False, indent=2))
PYEOF

echo
echo "| 口径 | 首帧 (中位, n=$RUNS) | 空载 RSS (KB) |"
echo "|---|---|---|"
echo "| gox 进程级冷启动到首帧 (真实窗口) | ${MED}ms | ${RSS} |"
echo
echo "结果已存档: $JSON"
