#!/usr/bin/env bash
# ============================================================================
# check-tree-fresh.sh — 开工前的"防返工闸门"：工作树必须站在 origin/main 上
#
# ## 为什么要有这个脚本
#
# 2026-11-17/18 的两夜, 有两个会话各自在**同一个已经废弃的本地分支**上开工:
#
#   7d6171d  perf(font): 族索引只读判定表          ← 只提交在本地, 从未推送
#   624042c  wip: 判定视图层的并发/阈值打磨        ← 紧接着在这上面继续改
#
# 而与此同时远端已经把这条路线**用数据否掉了** (bce8633: 视图层建立慢 2.5x,
# 读字节还更多), 并改走"并发化 + 自校准比值闸门"。也就是说第二夜的全部工作
# (含 A/B 测量) 从第一分钟起就是无效功 —— 不是做得不好, 是**前提在远端已经被
# 推翻, 而本地没有任何一处会提醒这件事**。
#
# 根因很朴素: `git status` 只说"你改了哪些文件", 不说"你的 HEAD 是不是已经
# 落后于远端"。落后的树在任何工具里都长得和正常的树一模一样。
#
# 所以开工前的第一件事应该是问一句"我站的地方对吗", 而不是问"我要改什么"。
# 这个脚本就是那句话, 做成可判定的: 落后即红, 脏树即红。
#
# ## 三条判据
#
#   ① HEAD 落后 origin/main        → 红 (列出差了哪些提交)。这是最贵的错:
#      所有改动都要在 rebase 之后重做一遍 A/B, 而很多人会直接跳过重测。
#   ② 已跟踪文件有未提交改动        → 红。脏树上构建的二进制 vcs.modified=true,
#      不能当 A/B 基准 (见 cmd/gox/buildinfo.go); 而"先提交再说"会把废改动
#      也带进历史。
#   ③ 本地领先 origin/main          → 黄 (不是错)。列出未推送提交: 它们要么
#      是今晚的成果 (该推), 要么是上一夜的孤儿 (该核销)。**不判红是因为**
#      开工时领先是正常的; 但必须被看见 —— 上一夜的孤儿提交正是这样漏掉的。
#
# ## 用法
#
#   ./scripts/check-tree-fresh.sh            # 用本地已有的 origin/main 引用
#   ./scripts/check-tree-fresh.sh --fetch    # 先 git fetch (需要网络)
#   ./scripts/check-tree-fresh.sh --self-test # 12 条自测: 造临时仓库逐条验证
#
# 退出码: 0 = 可以开工, 1 = 不许开工 (有红项), 2 = 用法错误
# ============================================================================
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
UPSTREAM="${UPSTREAM:-origin/main}"
FETCH=0
REPO=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    --fetch) FETCH=1; shift ;;
    --self-test) SELF_TEST=1; shift ;;
    --upstream) UPSTREAM="$2"; shift 2 ;;
    --repo) REPO="$2"; shift 2 ;;
    -h|--help) sed -n '2,50p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) echo "未知参数 $1 (见 --help)" >&2; exit 2 ;;
  esac
done

# ---------------------------------------------------------------------------
# 判据实现。全部以"当前目录就是被测仓库"为前提, 自测里靠 cd 切换。
# ---------------------------------------------------------------------------
RED=0

check_repo() {
  if ! git rev-parse --git-dir >/dev/null 2>&1; then
    echo "[fresh] ✗ 当前目录不是 git 仓库: $(pwd)" >&2
    return 1
  fi
}

check_upstream_ref() {
  if ! git rev-parse --verify --quiet "$UPSTREAM" >/dev/null; then
    echo "[fresh] ✗ 找不到远端引用 $UPSTREAM —— 先 git fetch, 或显式 --upstream <ref>" >&2
    return 1
  fi
}

# ① 落后判据: HEAD..upstream 有提交 = 落后的量。
check_behind() {
  local behind
  behind=$(git rev-list --count "HEAD..$UPSTREAM" 2>/dev/null || echo 0)
  if [[ "$behind" -gt 0 ]]; then
    echo "[fresh] ✗ HEAD 落后 $UPSTREAM $behind 个提交 —— 不许在上面开工" >&2
    echo "[fresh]   落后的提交 (从远到近):" >&2
    git log --oneline --no-decorate "HEAD..$UPSTREAM" | sed 's/^/[fresh]     /' >&2
    echo "[fresh]   处置: git reset --hard $UPSTREAM（未提交改动先存档到备份分支）" >&2
    return 1
  fi
  return 0
}

# ② 脏树判据: 只看已跟踪文件。未跟踪的构建产物(二进制/临时目录)不算脏 ——
# 这个仓库日常工作里就有 gox 二进制、*.test 落在树里, 判红会天天假红。
check_clean() {
  local dirty
  dirty=$(git status --porcelain --untracked-files=no)
  if [[ -n "$dirty" ]]; then
    echo "[fresh] ✗ 已跟踪文件有未提交改动 (脏树产出的二进制不能作 A/B 基准):" >&2
    echo "$dirty" | sed 's/^/[fresh]     /' >&2
    return 1
  fi
  return 0
}

# ③ 领先判据: 只提示。孤儿提交 (既不在远端、也不在今晚的工作里) 就是这样被看见的。
check_ahead() {
  local ahead
  ahead=$(git rev-list --count "$UPSTREAM..HEAD" 2>/dev/null || echo 0)
  if [[ "$ahead" -gt 0 ]]; then
    echo "[fresh] ! HEAD 领先 $UPSTREAM $ahead 个提交 (未推送):" >&2
    git log --oneline --no-decorate "$UPSTREAM..HEAD" | sed 's/^/[fresh]     /' >&2
    echo "[fresh]   要么推上去, 要么核销掉 —— 别让它在下一夜变成别人的前提" >&2
  fi
  return 0
}

run_checks() {
  RED=0
  cd "$REPO" || { echo "[fresh] ✗ 仓库路径不存在: $REPO" >&2; return 1; }
  check_repo || return 1
  check_upstream_ref || return 1
  check_behind || RED=1
  check_clean || RED=1
  check_ahead
  [[ "$RED" -eq 0 ]]
}

# ---------------------------------------------------------------------------
# --self-test: 造一个 bare "远端" + 一个 clone, 逐条把判据逼红/逼绿。
#
# 负向自测是必须的: 一个只会说"通过"的闸门, 和删掉没区别。这里每条红判据
# 都要在自测里真的红一次, 否则脚本改坏了也没人知道。
# ---------------------------------------------------------------------------
self_test() {
  local tmp pass=0 fail=0 SCRIPT
  SCRIPT="$ROOT/scripts/check-tree-fresh.sh"
  tmp=$(mktemp -d)
  # shellcheck disable=SC2064
  trap "rm -rf '$tmp'" EXIT

  # 身份走环境变量: 沙箱/CI 里普遍没有全局 user.name, 而 -c 只在命令行拼对时生效。
  export GIT_AUTHOR_NAME=selftest GIT_AUTHOR_EMAIL=selftest@local
  export GIT_COMMITTER_NAME=selftest GIT_COMMITTER_EMAIL=selftest@local

  git init -q --bare "$tmp/origin.git"
  git clone -q "$tmp/origin.git" "$tmp/w" 2>/dev/null
  ( cd "$tmp/w" && git branch -M main && echo one > a.txt && git add a.txt && \
    git commit -q -m c1 && git push -q -u origin main )

  expect() { # expect <ok|red> <说明> <args...>
    local want="$1" desc="$2"; shift 2
    local out rc
    out=$(bash "$SCRIPT" --repo "$tmp/w" "$@" 2>&1); rc=$?
    if { [[ "$want" == ok ]] && [[ $rc -eq 0 ]]; } || { [[ "$want" == red ]] && [[ $rc -ne 0 ]]; }; then
      echo "  ✓ $desc"; pass=$((pass+1))
    else
      echo "  ✗ $desc (期望 $want, 实际 rc=$rc)"; echo "$out" | sed 's/^/      /'; fail=$((fail+1))
    fi
  }

  echo "[fresh] 自测开始 ($tmp)"
  expect ok  "干净且与远端同点 → 放行"
  ( cd "$tmp/w" && touch junk.test )
  expect ok  "未跟踪产物 (.test) 不算脏 → 放行"
  ( cd "$tmp/w" && echo two >> a.txt )
  expect red "已跟踪文件有改动 → 拦下"
  ( cd "$tmp/w" && git checkout -q -- a.txt && echo x > b.txt && git add b.txt && git commit -q -m c2-local )
  expect ok  "本地领先但干净 → 放行 (只提示)"
  ( cd "$tmp/w" && git push -q origin main && git reset -q --hard HEAD~1 )
  expect red "落后远端 → 拦下"
  expect red "upstream 引用不存在 → 拦下" --upstream origin/nope
  ( cd "$tmp/w" && git reset -q --hard origin/main )
  mkdir -p "$tmp/plain"
  local out rc
  out=$(bash "$SCRIPT" --repo "$tmp/plain" 2>&1); rc=$?
  if [[ $rc -ne 0 ]]; then echo "  ✓ 非 git 目录 → 拦下"; pass=$((pass+1));
  else echo "  ✗ 非 git 目录 → 拦下"; echo "$out" | sed 's/^/      /'; fail=$((fail+1)); fi

  echo "[fresh] 自测结果: $pass 通过, $fail 失败"
  [[ "$fail" -eq 0 ]]
}

if [[ "${SELF_TEST:-0}" -eq 1 ]]; then
  self_test
  exit $?
fi

[[ -n "$REPO" ]] || REPO="$ROOT"

if [[ "$FETCH" -eq 1 ]]; then
  git -C "$REPO" fetch --prune origin >&2 || { echo "[fresh] ✗ git fetch 失败" >&2; exit 1; }
fi

run_checks
rc=$?
if [[ $rc -eq 0 ]]; then
  echo "[fresh] ✓ 可以开工: $(git -C "$REPO" rev-parse --short HEAD) == $UPSTREAM"
fi
exit $rc
