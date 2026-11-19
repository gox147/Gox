#!/usr/bin/env bash
# ============================================================================
# perf-ab.sh — 同机配对 A/B：把「这版真的更快吗」变成一个可复现的判定 (T3-4 第九批)
#
# 为什么需要它：本仓每一条热点提交都要求「自带 bench 前后数字」，但此前每个
# 批次都是手工敲两遍 go test 再自己心算 —— 两台机器/两个时间点的 ns/op 直接
# 相减会被机器漂移吃掉（实测同一份代码在 10 分钟跨度里 ns/op 能从 128ms 漂到
# 141ms，±10%）。那种数字既不能证明收益，也不能证明没有收益。
#
# 本工具做三件事，缺一不可：
#   1. **同机交替**：A/B 两棵树的基准二进制在同一次运行里交替取样
#      (A,B,A,B…)，机器漂移对两边同等生效，差值才是代码差值。
#   2. **带身份**：两侧都记录 commit 与 dirty 标记（脏树必须显式报出来 ——
#      脏树产出的「优化」无法回溯到某个提交，不能作为合并依据）。
#   3. **带阈值**：中位改善低于 --min-gain（默认 5%，与 T3-4 验收口径一致）
#      直接 exit 1。所以它既能当测量工具，也能当合并门。
#
# 口径与边界：
#   - 测的是**进程内微基准**（tools/perf），不是进程级 wall —— 后者含 exec 与
#     Go runtime 启动，改动哪怕 5% 也会被噪声盖住。冷启动走 scripts/coldstart.sh。
#   - 每次取样是**独立进程**（-test.count 1），避免同进程内的堆状态互相影响；
#     交替顺序抵消漂移，不抵消「同一侧内部」的抖动 —— 所以至少 5 轮。
#   - base 侧永远是一棵**干净 checkout**（git worktree 到 /tmp）；candidate 侧
#     是当前工作树（可以脏，dirty 会写进结果 JSON 与表格）。
#
# 跑法:
#   ./scripts/perf-ab.sh                                  # 默认 Fib28PureEval, 6 轮
#   ./scripts/perf-ab.sh --bench SetupGlobals --runs 8
#   ./scripts/perf-ab.sh --base origin/main --benchtime 20x
#   ./scripts/perf-ab.sh --no-gate                        # 只看数, 不按阈值退码
#
# 结果: bench-results/perf-ab-<UTC时间>.json + stdout 一张表。
# 默认 base = HEAD（未提交改动与 HEAD 比）；--base <rev> 可与任意提交比。
# ============================================================================
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BASE_REV="HEAD"
BENCH="Fib28PureEval"
RUNS=6
BENCHTIME="10x"
PKG="./tools/perf"
MIN_GAIN=5
GATE=1
KEEP=0

while [[ $# -gt 0 ]]; do
  case "$1" in
    --base) BASE_REV="$2"; shift 2 ;;
    --bench) BENCH="$2"; shift 2 ;;
    --runs) RUNS="$2"; shift 2 ;;
    --benchtime) BENCHTIME="$2"; shift 2 ;;
    --pkg) PKG="$2"; shift 2 ;;
    --min-gain) MIN_GAIN="$2"; shift 2 ;;
    --no-gate) GATE=0; shift ;;
    --keep) KEEP=1; shift ;;
    -h|--help) sed -n '2,40p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) echo "未知参数 $1 (见 --help)"; exit 1 ;;
  esac
done

if [[ "$RUNS" -lt 3 ]]; then
  echo "[perf-ab] --runs 至少 3（交替取样少于 3 轮无法给出可信中位）" >&2
  exit 2
fi

WORK="$(mktemp -d "${TMPDIR:-/tmp}/gox-ab-XXXXXX")"
cleanup() {
  if [[ "$KEEP" == "1" ]]; then
    echo "[perf-ab] 保留工作目录: $WORK"
    return
  fi
  git -C "$ROOT" worktree remove --force "$WORK/base" >/dev/null 2>&1 || true
  rm -rf "$WORK"
}
trap cleanup EXIT

BASE_SHA="$(git -C "$ROOT" rev-parse --verify --short "$BASE_REV")"
HEAD_SHA="$(git -C "$ROOT" rev-parse --short HEAD)"
BASE_DIRTY=false
HEAD_DIRTY=false
if [[ -n "$(git -C "$ROOT" status --porcelain)" ]]; then HEAD_DIRTY=true; fi

echo "==================================================================="
echo "[perf-ab] 同机配对 A/B"
echo "  时间(UTC):   $(date -u +%Y-%m-%dT%H:%M:%SZ)"
echo "  base:        $BASE_REV ($BASE_SHA, dirty=$BASE_DIRTY)"
echo "  candidate:   工作树 ($HEAD_SHA, dirty=$HEAD_DIRTY)"
echo "  基准:        $PKG -bench $BENCH -benchtime $BENCHTIME, 交替 $RUNS 轮"
echo "  判据:        中位改善 ≥ ${MIN_GAIN}% (gate=$GATE)"
echo "  机器:        $(nproc) 核 / $(/usr/bin/grep -m1 'model name' /proc/cpuinfo 2>/dev/null | cut -d: -f2 | xargs || echo n/a)"
echo "  Go:          $(go version)"
echo "==================================================================="

# base 侧: 干净 checkout（工作树里未提交的改动不会混进来）
git -C "$ROOT" worktree add --detach "$WORK/base" "$BASE_REV" >/dev/null
( cd "$WORK/base" && GOFLAGS="-mod=mod" go test -c -o "$WORK/base.test" "$PKG" >/dev/null )

# candidate 侧: 当前工作树（含未提交改动）
( cd "$ROOT" && go test -c -o "$WORK/head.test" "$PKG" >/dev/null )

run_once() { # $1=binary $2=outfile
  "$1" -test.run '^$' -test.bench "$BENCH" -test.benchtime "$BENCHTIME" -test.count 1 \
    > "$2" 2>&1 || { echo "[perf-ab] 基准执行失败（$1）:" >&2; cat "$2" >&2; exit 3; }
}

echo
echo "[perf-ab] 交替取样中 (A=base, B=candidate)…"
for i in $(seq 1 "$RUNS"); do
  run_once "$WORK/base.test" "$WORK/a-$i.txt"
  run_once "$WORK/head.test" "$WORK/b-$i.txt"
  printf '  轮 %-2s ' "$i"
  printf 'A=%-10s ' "$(awk '/ns\/op/{print $3}' "$WORK/a-$i.txt" | head -1)"
  printf 'B=%-10s\n' "$(awk '/ns\/op/{print $3}' "$WORK/b-$i.txt" | head -1)"
done

python3 - "$ROOT" "$WORK" "$RUNS" "$BASE_REV" "$BASE_SHA" "$HEAD_SHA" "$BASE_DIRTY" "$HEAD_DIRTY" "$BENCH" "$BENCHTIME" "$MIN_GAIN" "$GATE" <<'PY'
import json, os, re, statistics, subprocess, sys, time

(root, work, runs, base_rev, base_sha, head_sha, base_dirty, head_dirty,
 bench, benchtime, min_gain, gate) = sys.argv[1:13]
runs = int(runs); min_gain = float(min_gain); gate = gate == "1"

def parse(path):
    ns, allocs, bop = [], [], []
    for line in open(path):
        m = re.search(r"(\d+)\s+([\d.]+) ns/op(?:\s+(\d+) B/op)?(?:\s+(\d+) allocs/op)?", line)
        if m:
            ns.append(float(m.group(2)))
            bop.append(int(m.group(3)) if m.group(3) else 0)
            allocs.append(int(m.group(4)) if m.group(4) else 0)
    if not ns:
        print(f"[perf-ab] 解析不到 ns/op: {path}", file=sys.stderr)
        sys.exit(3)
    return ns, bop, allocs

A, B = [], []
for i in range(1, runs + 1):
    a = parse(os.path.join(work, f"a-{i}.txt"))
    b = parse(os.path.join(work, f"b-{i}.txt"))
    A.append(a); B.append(b)

def flatten(samples, k):
    return [v for s in samples for v in s[k]]

a_ns, b_ns = flatten(A, 0), flatten(B, 0)
a_b, b_b = flatten(A, 1), flatten(B, 1)
a_al, b_al = flatten(A, 2), flatten(B, 2)

med_a, med_b = statistics.median(a_ns), statistics.median(b_ns)
gain = (med_a - med_b) / med_a * 100.0
# 配对符号检验: 逐轮"同轮两值"比较, 抵消机器漂移
wins = sum(1 for i in range(runs) if statistics.median(B[i][0]) < statistics.median(A[i][0]))
med_a_al, med_b_al = statistics.median(a_al), statistics.median(b_al)

print()
print("-------------------------------------------------------------------")
print(f"{'指标':<16}{'base':>14}{'candidate':>14}{'变化':>12}")
print("-------------------------------------------------------------------")
print(f"{'ns/op (中位)':<14}{med_a:>14,.0f}{med_b:>14,.0f}{(med_b/med_a-1)*100:>11.2f}%")
print(f"{'B/op (中位)':<14}{statistics.median(a_b):>14,.0f}{statistics.median(b_b):>14,.0f}"
      f"{(statistics.median(b_b)/max(statistics.median(a_b),1)-1)*100:>11.2f}%")
print(f"{'allocs/op (中位)':<12}{med_a_al:>14,.0f}{med_b_al:>14,.0f}"
      f"{(med_b_al/max(med_a_al,1)-1)*100:>11.2f}%")
print("-------------------------------------------------------------------")
print(f"逐轮更快: {wins}/{runs} 轮（candidate 胜）")

verdict = "PASS(达标)" if gain >= min_gain else "FAIL(未达阈值)"
print(f"判定: 中位改善 {gain:+.2f}%，阈值 {min_gain:.2f}% ⇒ {verdict}")

out = {
    "tool": "perf-ab.sh",
    "generated_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
    "bench": bench,
    "benchtime": benchtime,
    "runs": runs,
    "base": {"rev": base_rev, "commit": base_sha, "dirty": base_dirty == "true"},
    "candidate": {"commit": head_sha, "dirty": head_dirty == "true"},
    "ns_per_op": {"base_median": med_a, "candidate_median": med_b, "gain_pct": gain},
    "b_per_op": {"base_median": statistics.median(a_b), "candidate_median": statistics.median(b_b)},
    "allocs_per_op": {"base_median": med_a_al, "candidate_median": med_b_al},
    "rounds_candidate_faster": wins,
    "threshold_pct": min_gain,
    "verdict": "pass" if gain >= min_gain else "fail",
    "samples": {"base": a_ns, "candidate": b_ns},
}
res_dir = os.path.join(root, "bench-results")
os.makedirs(res_dir, exist_ok=True)
path = os.path.join(res_dir, "perf-ab-" + time.strftime("%Y%m%d-%H%M%S", time.gmtime()) + ".json")
with open(path, "w") as f:
    json.dump(out, f, indent=2, ensure_ascii=False)
    f.write("\n")
print(f"结果: {path}")

if gate and gain < min_gain:
    sys.exit(1)
PY
