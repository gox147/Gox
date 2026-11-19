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
#   ./scripts/coldstart.sh                # 默认 7 次取中位数 (样本间隔 0.25s)
#   ./scripts/coldstart.sh --runs 15      # 加密重复
#   ./scripts/coldstart.sh --interval 0.3 # 样本间隔秒数 (默认 0.25, 见下)
#   ./scripts/coldstart.sh --user-form    # = --interval 0: 用户形态 (关掉就立刻再开)
#   ./scripts/coldstart.sh --bin <path>   # 测已有二进制 (跳过构建), 用于对比两份产物
#   ./scripts/coldstart.sh --xvfb         # 无 DISPLAY 时强制用 Xvfb 虚拟显示
#
# --- 样本间隔 (T3-5 第十二批, 2026-11-19; **第二十一批量化订正**) ---
#
# 旧版背靠背连跑, 每个样本都被"上一个 gox 刚断开 X 连接"的服务器侧清理
# 推迟: 实测同一个纯 xgb 连接程序 (只 dial + 握手, 不建窗口) 连跑三次
# 是 0.25ms / 19.3ms / 19.2ms, 间隔 0.3s 后恢复到 0.2ms 量级。也就是说旧口径
# 测到的"38ms"里约 14ms 是 Xvfb 的 accept 延迟伪影, 不是 gox 的成本。
# **这是测量口径修正, 不是性能优化**: 新旧数字不可相减, 引用时必须带口径。
#
# 第二十一批把"到底要等多久"量出来了 (同一二进制, n=9, Xvfb :55, 见 §12.21.3):
#
#   | 间隔  | 首帧中位 |     | 间隔  | 首帧中位 |
#   | 0     | 16~18ms  |     | 30ms  | 8~9ms   |
#   | 5ms   | 12~13ms  |     | 250ms | 9ms     |
#   | 10ms  | 8~9ms    |     | 1000ms| 9ms     |
#
# 也就是这个伪影的窗口只有 **5~10ms** (上一批曾以为要等 ~1s, 那是把"1.2s 够用"
# 读成了"窗口有 1s")。默认 0.25s 因此是**富余**而非必要 —— 它从 2026-11-19 起
# 就一直生效, 历史样本没有这个问题 (上一批"间隔未生效"的结论已撤回, §12.21.0)。
#
# 两种口径都合法, 但必须说清是哪种:
#   - 默认 (间隔 0.25s): 干净口径, 用于回归盯梢与"引擎本身多快"。
#   - --user-form (间隔 0): 用户形态, 用户关掉窗口后**立刻**再双击就是这条路径,
#     它含那 9ms 握手抑制 —— 对外讲体感时用这个, 并说明差额来自 X server。
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
BIN=""          # --bin <path>: 测已有二进制, 跳过构建
# 样本间隔 (秒)。0 表示用户形态 (背靠背, 见文件头), 仅供复现历史数字与对外体感口径。
INTERVAL=0.25
SAMPLING_FORM="script"

GREP=/usr/bin/grep
[[ -x "$GREP" ]] || GREP=$(command -v grep)

while [[ $# -gt 0 ]]; do
  case "$1" in
    --runs) RUNS="$2"; shift 2 ;;
    --out)  OUT="$2"; shift 2 ;;
    --interval) INTERVAL="$2"; shift 2 ;;
    --user-form) INTERVAL=0; shift ;;
    --bin)  BIN="$2"; shift 2 ;;
    --xvfb) WANT_XVFB=1; shift ;;
    # 只打头部注释块: 跳过 shebang, 遇到第一行非注释就停 (固定行号会在改注释后错位)
    -h|--help) awk 'NR>1 && /^#/ {sub(/^# ?/, ""); print; next} NR>1 {exit}' "$0"; exit 0 ;;
    *) echo "未知参数 $1 (见 --help)"; exit 1 ;;
  esac
done
# 口径标签与间隔保持一致, 免得 JSON 里写着 script 而样本其实是背靠背。
if awk -v v="$INTERVAL" 'BEGIN{exit !(v <= 0)}'; then SAMPLING_FORM="user"; fi
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

# 第二十一批: 不再无条件信任 $DISPLAY —— 本沙箱里 DISPLAY=:0 常年存在但
# /tmp/.X11-unix/X0 已经没了 (容器重启留下的残留环境变量), 直接用它会让脚本
# 在建窗那一刻才失败, 报错还指向 JS 侧 (`connect: no such file or directory`)。
# 本地 unix socket 形态的 DISPLAY 先摸一下 socket 在不在, 不在就当没有。
display_usable() {
  local d=$1
  [[ -z "$d" ]] && return 1
  if [[ "$d" =~ ^:([0-9]+) ]]; then
    [[ -e "/tmp/.X11-unix/X${BASH_REMATCH[1]}" ]]
    return $?
  fi
  return 0  # host:N 形态无法本地判断, 交给客户端自己报错
}

if [[ -n "${DISPLAY:-}" && "$WANT_XVFB" -eq 0 ]] && display_usable "$DISPLAY"; then
  echo "[coldstart] 使用现有显示: DISPLAY=$DISPLAY" >&2
elif command -v Xvfb >/dev/null 2>&1; then
  [[ -n "${DISPLAY:-}" && "$WANT_XVFB" -eq 0 ]] && \
    echo "[coldstart] DISPLAY=$DISPLAY 不可用 (socket 不在), 改用自管 Xvfb" >&2
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
# --bin 给了二进制就跳过构建 (用来对比两份已建好的产物: 不同 ldflags / 两个提交)。
if [[ -n "$BIN" ]]; then
  [[ -x "$BIN" ]] || { echo "[coldstart] --bin $BIN 不存在或不可执行" >&2; exit 1; }
  GOX_BIN="$BIN"
  echo "[coldstart] 用给定二进制: $GOX_BIN" >&2
else
  echo "[coldstart] 构建本仓库 gox ..." >&2
  GOX_BIN="$OUT/gox-coldstart"
  (cd "$ROOT" && go build -o "$GOX_BIN" ./cmd/gox) >&2
fi

SCRIPT="$ROOT/scripts/bench/coldstart_gui.js"
[[ -f "$SCRIPT" ]] || { echo "[coldstart] 找不到 $SCRIPT" >&2; exit 1; }

# -------- 计量 --------
#
# 时钟用 bash 内建 $EPOCHREALTIME (微秒), **不再每个样本 fork 两次 `date`**。
# 第十七批实测: 两次 `date +%s%N` 的 fork/exec 落在 t1 那一侧, 会把 ~0.9ms
# 的支架开销记到 gox 头上 (同机同二进制: fork 版 9ms / 内建时钟版 8.1ms)。
# 只有 bash < 5 或变量被清掉时才退回 date。
# 时钟源: EPOCHREALTIME (bash 5+ 内建, 无 fork) 或 date (兜底)。自检失败才降级,
# 因为 date 每样本两次 fork 会把 ~0.9ms 记到 gox 头上 (见上)。
CLOCK_NAME="EPOCHREALTIME"
now_us() {
  if [[ "$CLOCK_NAME" == "EPOCHREALTIME" && -n "${EPOCHREALTIME:-}" ]]; then
    local t=${EPOCHREALTIME/[.,]/}
    printf '%s' "$((t))"
  else
    date +%s%6N
  fi
}

# -------- 支架自检 (第二十一批) --------
#
# 起因: 上一批 (§12.20.0) 曾断言"本机 sleep 的小数秒按整数截断, sleep 0.5 只等
# 1.2ms", 并据此把自 §12.12 起所有 250ms 间隔的历史样本判成"其实是背靠背"。
# 重测是**反过来的**: sleep 0.25 就是 251ms, 间隔一直是生效的 (§12.21.0)。
# 误判源头是手敲计时踩了两个坑: zsh 里没有 $EPOCHREALTIME (空串), 以及 `%6N`
# 是微秒、除 1000 得到的是**毫秒**却被当成微秒读。
#
# 所以"相信自己的支架"不能靠记性, 每次跑都在这里验两条, 不达标就**拒绝出数**:
#   1. 时钟必须前进;  2. 样本间隔必须真的等够 (实测 >= 请求值的 80%)。
# 宁可这次没有数字, 也不要一个"说了等 250ms 其实没等"的数字。
#
# 验时钟**不能用 sleep 当尺子**: 一台 sleep 坏掉的机器会让"时钟不前进"和
# "sleep 不等"得出同一现象, 报错就会指向错的组件 (第一版正是这么写错的)。
# 这里用一段纯 CPU 的忙循环 —— 它必然耗掉真实时间, 任何读数差为 0 的时钟就是坏的。
clock_advances() {
  local name=$1 t0 t1 i
  CLOCK_NAME=$name
  t0=$(now_us)
  for ((i = 0; i < 50000; i++)); do :; done
  t1=$(now_us)
  (( t1 > t0 ))
}
SELFTEST_T0=$(now_us)
if ! clock_advances EPOCHREALTIME; then
  # 内建时钟不可信就退回 `date` (代价是每样本多两次 fork, 见上面第十七批的注)
  echo "[coldstart] 内建 \$EPOCHREALTIME 不前进, 退回 date 计时" >&2
  if ! clock_advances date; then
    echo "[coldstart] ✗ 两个时钟源都不前进 (纯 CPU 忙循环前后读数相同) —— 本机耗时不可采样。" >&2
    echo "[coldstart]   先跑 bash scripts/check-harness-clock.sh 定位 (退出码 2 = 时钟, 1 = sleep)。" >&2
    exit 2
  fi
fi
CLOCK_SELFTEST_US=$(( $(now_us) - SELFTEST_T0 ))
CLOCK_SELFTEST_MS=$(( CLOCK_SELFTEST_US / 1000 ))

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
GAP_SUM_US=0
GAP_N=0
for i in $(seq 1 "$RUNS"); do
  ms=$(run_once_ms)
  SAMPLES+=("$ms")
  echo "[coldstart] 第 $i/$RUNS 次: ${ms}ms" >&2
  # 间隔见文件头 "样本间隔": 不等的话每个样本都会被 X server 的断开清理推迟。
  # 第二十一批: 顺手记下**实际**等了多久 —— 声明 250ms 而实际 1ms 的采样必须被判死,
  # 所以间隔不是"设置了就算数", 而是"量到了才算数"。
  if [[ "$i" -lt "$RUNS" ]]; then
    g0=$(now_us); sleep "$INTERVAL"; g1=$(now_us)
    GAP_SUM_US=$(( GAP_SUM_US + g1 - g0 )); GAP_N=$(( GAP_N + 1 ))
  fi
done

# 间隔自证: 请求了间隔就必须真的等够 (>=80%)。sleep 只会多睡不会少睡, 少睡说明
# 这个环境的 sleep 不遵守小数秒 —— 那种环境下"间隔型样本"整体无效, 拒绝出数。
INTERVAL_MS_EFF=0
if (( GAP_N > 0 )); then
  INTERVAL_MS_EFF=$(( (GAP_SUM_US / GAP_N + 500) / 1000 ))
  FLOOR_MS=$(awk -v r="$INTERVAL" 'BEGIN{printf "%d", r*1000*0.8}')
  if (( INTERVAL_MS_EFF < FLOOR_MS )); then
    echo "[coldstart] ✗ 请求的样本间隔是 ${INTERVAL}s, 实测平均只有 ${INTERVAL_MS_EFF}ms (<80%)。" >&2
    echo "[coldstart]   时钟已验证会前进, 所以是本机 sleep 不遵守请求时长 —— 这批数字不可用, 已丢弃。" >&2
    echo "[coldstart]   先跑 bash scripts/check-harness-clock.sh; 临时可用 --interval 1 (整秒) 重跑。" >&2
    exit 2
  fi
fi

MED=$(median "${SAMPLES[@]}")
RSS=$(run_rss_kb)

# -------- JSON + 表 --------
STAMP=$(date -u +%Y%m%d-%H%M%S)
# 文件名只到秒 —— 小间隔 (0.01s) 连跑两档会在同一秒里落盘, 后一份**静默覆盖**
# 前一份 (第二十一批实测撞过: 0.01 与 0.03 两档只剩下一份结果)。撞名就加序号。
JSON="$OUT/coldstart-process-$STAMP.json"
_dup=1
while [[ -e "$JSON" ]]; do JSON="$OUT/coldstart-process-$STAMP-$_dup.json"; _dup=$((_dup + 1)); done
GOX_COLDSTART_CLOCK="$([[ "$CLOCK_NAME" == "EPOCHREALTIME" ]] && echo "bash-epochrealtime(us, no fork)" || echo "date(us, 2 forks/sample)")"
GOX_COLDSTART_DISPLAY="$DISPLAY_MODE"
export GOX_COLDSTART_CLOCK GOX_COLDSTART_DISPLAY
python3 - "$JSON" "$MED" "$RSS" "$RUNS" "${SAMPLES[*]}" "$INTERVAL" \
         "$SAMPLING_FORM" "$INTERVAL_MS_EFF" "$CLOCK_SELFTEST_MS" <<'PYEOF'
import sys, json, datetime, os, platform, subprocess

json_path, med, rss, runs, samples = sys.argv[1], int(sys.argv[2]), int(sys.argv[3]), int(sys.argv[4]), [int(x) for x in sys.argv[5].split()]
interval_ms = int(round(float(sys.argv[6]) * 1000))
sampling_form = sys.argv[7]                 # script | user
interval_eff_ms = int(sys.argv[8])          # 实测平均间隔（不是请求值）
clock_selftest_ms = int(sys.argv[9])        # 跨 sleep 0.05 的读数差, 应为 ~50ms
interval_honored = True if interval_ms == 0 else interval_eff_ms >= interval_ms * 0.8

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
    "interval_ms_effective": interval_eff_ms,
    "interval_honored": interval_honored,
    "sampling_form": sampling_form,
    "harness": {
        "clock": os.environ.get("GOX_COLDSTART_CLOCK", "unknown"),
        "clock_selftest_ms": clock_selftest_ms,
        "display_mode": os.environ.get("GOX_COLDSTART_DISPLAY", "unknown"),
        "coldstart_sh_rev": "T3-5-21 (2026-11-20): 时钟/间隔自证 —— 间隔实测不到请求值 80% 直接拒绝出数; --user-form 显式标成用户形态",
        "note": "2026-11-19 第十七批之前的样本含 ~0.9ms `date` fork 与(在 --xvfb 模式下)~30ms 的 Xvfb 启动成本, 与本节之前的历史数字不可直接相减; 见 docs/performance.md §12.17",
    },
    "method": "外部 wall time (进程 exec → 脚本打印 FIRST_FRAME → 进程退出, 含 ≤2ms 探测与 ~0.7ms 退出清理, 见 §12.17), 跑 %d 次取中位数, 预热 2 次不计入, 样本间隔 %dms (实测平均 %dms; 避开 X server 断开清理造成的握手抑制, 见脚本头与 §12.21.3); 首帧信号 = JS 侧 1ms 轮询 gx/dev 的 devSnapshot().frame.count (只在真实上屏出口计数)" % (runs, interval_ms, interval_eff_ms),
    "scope": "进程级冷启动到首帧: 含进程 exec/动态链接/Go runtime 启动/解释器初始化/脚本求值(含模块装载)/真实窗口创建/首次 Layout+Draw+上屏",
    "host": {
        "platform": platform.platform(),
        "machine": platform.machine(),
        "cpu": cpu,
        "cores": os.cpu_count(),
        "display": os.environ.get("DISPLAY", ""),
        "go": sh("go version") or "go (构建本二进制的工具链)",
    },
    "note": "对外体感口径用 --user-form 那一份 (sampling_form=user, 区间 0), 引擎口径用默认那份 (sampling_form=script); 两者差 ~9ms 且差额全部来自 X server 的握手抑制, 不是 gox 的代码差异 (§12.21.3)。与 tools/perf 的进程内口径 (coldstart-*.json) 不可混用",
}
json.dump(doc, open(json_path, "w"), ensure_ascii=False, indent=2)
print(json.dumps(doc, ensure_ascii=False, indent=2))
PYEOF

echo
echo "| 口径 | 首帧 (中位, n=$RUNS) | 空载 RSS (KB) |"
echo "|---|---|---|"
if [[ "$SAMPLING_FORM" == "user" ]]; then
  echo "| gox 进程级冷启动到首帧 (真实窗口, **用户形态** 背靠背) | ${MED}ms | ${RSS} |"
else
  echo "| gox 进程级冷启动到首帧 (真实窗口, 间隔 ${INTERVAL_MS_EFF}ms) | ${MED}ms | ${RSS} |"
fi
echo
echo "结果已存档: $JSON"
