#!/usr/bin/env bash
# ============================================================================
# install-hooks.sh — 装上本仓的本地提交钩子（commit-msg: 性能 A/B 契约）
#
# 为什么需要：docs/performance.md §3.4.6 起，"每条性能提交自带同机 A/B 数字"
# 是本仓纪律；但纪律写在文档里不会拦住任何人。2026-11-20 夜批的实况是：四条
# 候选三条被数字否掉、第四条连 vm 全量测试都过不了，而"实验代码有没有留在树里"
# "否掉的候选有没有带数字"全靠人记性。
#
# tools/abcontract 把这条纪律变成可判定的；本脚本把它挂到提交那一刻。钩子只
# 校验**提交信息**（不跑基准、不碰网络），亚秒级，失败信息直接给出缺什么。
#
# 用法:  ./scripts/install-hooks.sh          # 装到 .git/hooks/commit-msg
#        ./scripts/install-hooks.sh --remove # 卸掉
# ============================================================================
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
HOOK="$ROOT/.git/hooks/commit-msg"

if [[ "${1:-}" == "--remove" ]]; then
  rm -f "$HOOK"
  echo "[hooks] 已卸下 commit-msg"
  exit 0
fi

if [[ ! -d "$ROOT/.git/hooks" ]]; then
  echo "[hooks] 不是 git 仓库: $ROOT" >&2
  exit 1
fi

cat > "$HOOK" <<'EOF'
#!/usr/bin/env bash
# 由 scripts/install-hooks.sh 安装；判据在 tools/abcontract（带自测）。
# 绕过: git commit --no-verify（只该用于非性能提交的紧急情况）。
set -euo pipefail
root="$(git rev-parse --show-toplevel)"
if ! ( cd "$root" && go run ./tools/abcontract -message "$1" ); then
  echo "[commit-msg] 性能 A/B 契约未满足 —— 见上一条逐项说明；" >&2
  echo "[commit-msg] 判据与自测: go run ./tools/abcontract -self-test" >&2
  exit 1
fi
EOF
chmod +x "$HOOK"
echo "[hooks] 已安装 commit-msg -> tools/abcontract（--remove 可卸下）"
