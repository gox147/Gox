#!/usr/bin/env bash
# ============================================================================
# gpu-track-audit.sh — gfx/gpu 三轨现状核销 (G-1)
#
# 逐轨验证「这台机器上每条构建轨实际走哪条路径」, 并做交叉编译矩阵抽查。
# 输出一张机器可读的核销表; 配合 scripts/bench-present-4k.sh 的对照数字,
# 即为 G-1 的验收材料 (表格模板见 docs/gpu-track-audit.md)。
#
# 口径纪律 (与本仓库 bench-present-4k.sh 同源):
#   - 无 GPU 硬件机器上 wgpu 轨 kind=software —— **兜底口径**: 不进闸门、
#     不对外; 有硬件机器上重跑本脚本覆盖对应行, 才是可信数字。
#   - nogpu / cgo(骨架) 轨恒不做呈现包装, 与硬件无关。
#
# 用法: ./scripts/gpu-track-audit.sh            # 三轨逐项核销
#       ./scripts/gpu-track-audit.sh --xcompile # 追加交叉编译矩阵抽查
# ============================================================================
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

XCOMPILE=0
[[ "${1:-}" == "--xcompile" ]] && XCOMPILE=1

row() { # track platform kind backend available extra...
  printf '| %s | %s | %s | %s | %s | %s |\n' "$1" "$2" "$3" "$4" "$5" "${6:--}"
}

echo "GPU 三轨核销 ($(date '+%F %T'), host: $(uname -sm), gpu probe below)"
echo "| 轨 | 平台 | BackendKind | Backend | Available | 备注 |"
echo "|---|---|---|---|---|---|"

# ---- 逐轨探针: gfx/gpu/probe_*_test.go 的 TestBackendTrackReport ----
probe() { # extra_tags
  go test "./gfx/gpu" -run '^TestBackendTrackReport$' -v $1 2>&1 \
    | grep -E 'TRACK=|FAIL|build constraints|cgo: C compiler' | head -4
}

# 1. 默认轨 (nogpu 空壳)
out=$(probe "")
if grep -q 'TRACK=nogpu' <<<"$out"; then
  echo "$out" | grep 'TRACK=nogpu' | while read -r line; do
    row nogpu "${GOOS:-$(go env GOHOSTOS)}" none none false "恒不包装, CPU blit"
  done
else
  row nogpu "$(go env GOHOSTOS)" ERROR probe-failed false "探针未输出: ${out:0:80}"
fi

# 2. wgpu 轨 (-tags gpu)
out=$(probe "-tags gpu")
if grep -q 'TRACK=wgpu' <<<"$out"; then
  line=$(grep 'TRACK=wgpu' <<<"$out" | head -1)
  kind=$(sed -n 's/.*kind=\([^ ]*\).*/\1/p' <<<"$line")
  backend=$(sed -n 's/.*backend=\([^ ]*\).*/\1/p' <<<"$line")
  av=$(sed -n 's/.*available=\([^ ]*\).*/\1/p' <<<"$line")
  note="生产只在 kind=hardware 包装; software=兜底口径不进闸门"
  grep -q 'note=fallback' <<<"$out" && note="无硬件兜底口径: 不进闸门、不对外"
  row wgpu "$(go env GOHOSTOS)" "$kind" "$backend" "$av" "$note"
else
  row wgpu "$(go env GOHOSTOS)" ERROR probe-failed false "${out:0:80}"
fi

# 3. cgo 轨 (-tags gpu_cgo; 骨架阶段恒不可用)
out=$(probe "-tags gpu_cgo")
if grep -q 'TRACK=cgo' <<<"$out"; then
  line=$(grep 'TRACK=cgo' <<<"$out" | head -1)
  kind=$(sed -n 's/.*kind=\([^ ]*\).*/\1/p' <<<"$line")
  row cgo "$(go env GOHOSTOS)" "$kind" cgo false "骨架阶段恒 ErrNotBuilt"
else
  row cgo "$(go env GOHOSTOS)" ERROR probe-failed false "构建失败 (需 C 工具链): ${out:0:60}"
fi

# ---- 接入面核销 (静态): SetPresentWrapper 接入点 / 移动端 ----
wrapper_files=$(grep -rln 'SetPresentWrapper' cmd gfx --include='*.go' | grep -v _test | tr '\n' ' ')
echo ""
echo "接入面核销:"
echo "  - SetPresentWrapper 接入点: ${wrapper_files:-无}"
echo "  - 移动端 (android/ios/harmony) gpu 接入: $(grep -rln 'gfx/gpu' gfx/android gfx/ios gfx/harmony --include='*.go' 2>/dev/null | head -1 || true)未接入"
echo "  - 窗口化上屏最后一跳: $(grep -c '在此代替 readback' gfx/gpu/backend_wgpu.go) 处 TODO (swapchain 未接, 仅离屏/回读验证)"

# ---- CI 核销 ----
gpu_ci=$(grep -rln -- '-tags gpu' .github/workflows/ 2>/dev/null | tr '\n' ' ')
echo "  - CI gpu 跑批: ${gpu_ci:-无 (见 .github/workflows/gpu-present.yml 接入方案)}"

# ---- 交叉编译矩阵抽查 (可选) ----
if [[ "$XCOMPILE" == 1 ]]; then
  echo ""
  echo "交叉编译矩阵抽查 (-tags gpu, 编译期验证 wgpu 纯 Go 可交叉):"
  for tgt in "windows/amd64" "darwin/arm64" "linux/arm64" "js/wasm"; do
    GOOS="${tgt%%/*}" GOARCH="${tgt##*/}" go build -tags gpu -o /dev/null ./cmd/gox \
      && echo "  ok   $tgt" || echo "  FAIL $tgt"
  done
fi
