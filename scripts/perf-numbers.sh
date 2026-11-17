#!/usr/bin/env bash
# ============================================================================
# perf-numbers.sh — 刷新对外性能数字（T3-7 单一真相源的「一条命令」入口）
#
#   ./scripts/perf-numbers.sh              # 从最新 bench-results 重建 golden 并重写 README
#   ./scripts/perf-numbers.sh --check      # 只校验（CI/PR 用，零依赖、亚秒级）
#   ./scripts/perf-numbers.sh --pr        # 同 --check，但额外打印「怎么修」的指引
#
# 顺序为什么固定为 record → write：
#   `--write` 只把 **golden**（docs/perf-numbers.json）渲染进文档，它自己不读原始
#   基准结果。只跑 --write 等于「用旧数字重排一遍」，看起来更新了、其实没更新 ——
#   这比什么都不做更危险。所以这里的默认动作是两步都跑。
#
# 什么时候该跑：只有**真的重跑了基准**之后。数字没变而文件变了，说明基准没跑成
#   （例如 bench-compare.sh 因缺少 node 跳过了对照栈），先看 bench-results 里新增
#   的 JSON 再决定要不要提交。
# ============================================================================
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
GEN="$ROOT/scripts/gen-perf-numbers.py"
MODE="refresh"
case "${1:-}" in
  --check) MODE="check" ;;
  --pr) MODE="pr" ;;
  "") ;;
  -h|--help) sed -n '2,20p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
  *) echo "未知参数: $1（可用: --check / --pr / 空）" >&2; exit 2 ;;
esac

fail_hint() {
  cat >&2 <<'EOF'

怎么修（按顺序）：
  1) 先跑基准拿到新数据：./scripts/bench-all.sh --runs 5
     （冷启动口径另跑：./scripts/coldstart.sh --runs 15）
  2) 重建 golden 并重写文档：./scripts/perf-numbers.sh
  3) 复核 diff：数字应只随第 1 步的结果变化；若表里出现 "—"，说明该栈这次没跑成
EOF
}

if [[ "$MODE" == "refresh" ]]; then
  python3 "$GEN" --record
  python3 "$GEN" --write
  python3 "$GEN" --check || { fail_hint; exit 1; }
  echo "[perf-numbers] 完成：golden 与 README 已同步（记得把 bench-results/ 的新 JSON 一起提交，数字才可追溯）"
  exit 0
fi

if ! python3 "$ROOT/scripts/test_gen_perf_numbers.py"; then
  echo "[perf-numbers] 自测失败：判据本身坏了，先修脚本再谈数字" >&2
  exit 1
fi
if ! python3 "$GEN" --check; then
  [[ "$MODE" == "pr" ]] && fail_hint
  exit 1
fi
echo "[perf-numbers] ok README 数字与 docs/perf-numbers.json 一致"
