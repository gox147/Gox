#!/usr/bin/env bash
# ============================================================================
# check-harness-clock.sh — 自检"这台机器的 sleep 与计时器会不会骗人"
# (T3-5 第二十一批, 2026-11-20 凌晨批)
#
# 为什么需要它: 上一批 (§12.20.0) 曾断言"本沙箱 sleep 的小数秒按整数截断,
# `sleep 0.5` 只过去 1.2ms", 并据此把所有历史样本判成"背靠背连跑"。该结论
# **是错的** —— 同一条命令在今天重跑过去 501ms。测量脚本会撒谎, 而撒谎的
# 测量脚本比没有测量更糟: 它会把"没生效的间隔"和"生效的间隔"混成一堆说不清
# 的数字。所以把"间隔/时钟是否可信"做成一个 2 秒能跑完的自检, 谁怀疑数字就
# 先跑它, 不用相信任何人的转述。
#
# 查两件事:
#   1. 小数秒 sleep 是否真的等够了 (GNU/BSD/macOS 的 sleep 都支持小数;
#      真不支持的环境会退回整秒 —— 那种机器上冷启动数字不可用);
#   2. 两种时钟源是否都在前进: bash 内建 $EPOCHREALTIME 与 `date +%s%6N`。
#      顺带说明一个坑: $EPOCHREALTIME 是 **bash 5+** 的变量, zsh 里根本没有
#      (要先 `zmodload zsh/datetime`), 在 zsh 里读它是空串 —— 上一批"两次相邻
#      取值给出 0"就是这么来的, 是 shell 的锅, 不是机器的锅。
#
# 用法:
#   ./scripts/check-harness-clock.sh            # 默认查 0.25 / 0.5 / 1.0 秒
#   ./scripts/check-harness-clock.sh 0.25 1.2   # 指定要查的时长 (秒)
#
# 退出码: 0 = 全部可信; 1 = 小数 sleep 不等够 (间隔会失效); 2 = 时钟不前进。
# ============================================================================
set -uo pipefail

DURATIONS=("$@")
if [[ ${#DURATIONS[@]} -eq 0 ]]; then
  DURATIONS=(0.25 0.5 1.0)
fi

GREP=/usr/bin/grep
[[ -x "$GREP" ]] || GREP=$(command -v grep)

# 时钟: 与 scripts/coldstart.sh 完全同款的两条路, 保证"自检的就是在用的"。
now_epochrealtime_us() {
  local t=${EPOCHREALTIME/[.,]/}
  printf '%s' "$((t))"
}
now_date_us() { date +%s%6N; }

echo "[harness] shell=$BASH_VERSION  sleep=$(command -v sleep)"
echo

BAD_SLEEP=0
BAD_CLOCK=0
printf '| 请求时长 | EPOCHREALTIME 实测 | date 实测 | 判定 |\n'
printf '|---|---|---|---|\n'
for d in "${DURATIONS[@]}"; do
  t0=$(now_epochrealtime_us); sleep "$d"; t1=$(now_epochrealtime_us)
  e_us=$((t1 - t0))
  t0=$(now_date_us); sleep "$d"; t1=$(now_date_us)
  d_us=$((t1 - t0))
  want_us=$(awk -v s="$d" 'BEGIN{printf "%d", s*1000000}')
  # sleep 只会多睡不会少睡 (内核定时器精度 + 调度延迟), 所以只判下界:
  # 低于请求值 80% 就是"没等够", 间隔失效。
  floor_us=$((want_us * 8 / 10))
  verdict="ok"
  (( e_us < floor_us )) && { verdict="EPOCHREALTIME 不等够"; BAD_SLEEP=1; }
  (( d_us < floor_us )) && { verdict="date 不等够"; BAD_SLEEP=1; }
  (( e_us <= 0 || d_us <= 0 )) && { verdict="时钟不前进"; BAD_CLOCK=1; }
  printf '| %ss | %.1fms | %.1fms | %s |\n' "$d" \
    "$(awk -v v="$e_us" 'BEGIN{print v/1000}')" \
    "$(awk -v v="$d_us" 'BEGIN{print v/1000}')" "$verdict"
done
echo

if (( BAD_CLOCK )); then
  echo "[harness] ✗ 至少一个时钟源不前进 —— 本机的采样间隔与耗时都不可信, 先修时钟再用结果。" >&2
  exit 2
fi
if (( BAD_SLEEP )); then
  echo "[harness] ✗ sleep 不遵守请求时长 (小数秒被吞) —— 样本间隔会失效, 别用这台机器的" >&2
  echo "[harness]   间隔型样本; 冷启动脚本会自己发现并拒绝出数 (见 scripts/coldstart.sh)。" >&2
  exit 1
fi

# 附加说明: zsh 里读 $EPOCHREALTIME 是空串 —— 上一批的误判源头, 记在这里免得下次重犯。
if command -v zsh >/dev/null 2>&1; then
  zsh_probe=$(zsh -c 'if [[ -z ${EPOCHREALTIME:-} ]]; then echo EMPTY; else echo "$EPOCHREALTIME"; fi' 2>/dev/null || echo "unavailable")
  if [[ "$zsh_probe" == "EMPTY" ]]; then
    echo "[harness] 提醒: zsh 下 \$EPOCHREALTIME 为空 (需 zmodload zsh/datetime) —— 手敲计时请用 bash 或 \`date +%s%6N\`。"
    echo
  fi
fi

echo "[harness] ok  时钟与小数秒 sleep 均可信 (n=${#DURATIONS[@]})"
exit 0
