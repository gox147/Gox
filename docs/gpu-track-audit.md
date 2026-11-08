# GPU 呈现三轨核销（G-1）

生成：`scripts/gpu-track-audit.sh`（探针：`gfx/gpu/probe_{nogpu,gpu,cgo}_test.go`）。
本表回答 G-1 的验收问题：三轨 × 有/无硬件 × 平台，各自实际走哪条路径。

## 口径纪律

- **无 GPU 硬件机器上的 wgpu 轨数字是兜底口径**（`kind=software`，
  `wgpu-software`）：不进 nightly 闸门（bench-all.sh）、不对外。4K 实测
  软件后端做合成比 CPU blit 慢约 5x，拿它说「我们做了 GPU」是自欺。
- 硬件可信口径须在带 `gpu-hardware` 标签的 runner（或本机真机）重跑
  `scripts/gpu-track-audit.sh` + `scripts/bench-present-4k.sh` 覆盖本表。

## 核销表（2026-11-08，Linux x86_64 无硬件沙箱；硬件行待真机覆盖）

| 轨 | 构建命令 | 无硬件路径 | 有硬件路径（预期，待真机证实） | 呈现包装 |
|---|---|---|---|---|
| nogpu（默认） | `go build ./cmd/gox` | kind=none，New 恒 ErrNotBuilt | 同左（与硬件无关） | 恒不包装 → CPU blit |
| wgpu | `go build -tags gpu` | kind=software，backend=wgpu-software（兜底口径） | kind=hardware，backend=wgpu-{vulkan,dx12,metal}-hw | 仅 hardware 包装；`GOX_GPU=software` 可强制 |
| cgo（骨架） | `-tags gpu_cgo` | kind=none，New 恒 ErrNotBuilt | 同左（绑定未补齐） | 恒不包装 |

实测探针输出（本机，无硬件）：

```text
TRACK=nogpu kind=none backend=none available=false new_err=ErrNotBuilt
TRACK=wgpu kind=software backend=wgpu-software available=true   ← 兜底口径
TRACK=cgo kind=none backend=cgo available=false                 ← 骨架纪律钉住
```

## 接入面核销（静态，与硬件无关）

| 项 | 现状 |
|---|---|
| SetPresentWrapper 接入点 | 仅 `cmd/gox/gpu_present_init.go`（桌面）；softmedia 链式叠加 |
| 移动端 android/ios/harmony | **未接入** SetPresentWrapper（grep 零命中） |
| **窗口化上屏最后一跳** | **未接 swapchain**：`backend_wgpu.go` ShowRegions 收尾留有
「surface.Present(surfaceTex) 在此代替 readback」的 TODO —— 当前 GPU 轨只
能离屏合成 + Readback 验证；「窗口化时由 swapchain 提供」的注释与代码不符 |
| CI gpu 跑批 | 此前无；已接入 `.github/workflows/gpu-present.yml` |

## 关键结论：A 路径（上屏合成默认化）的硬前提缺口

gogpu/wgpu 已提供原生窗口接入 API（`SurfaceTargetFromXlibWindow` /
`SurfaceTargetFromWindowsHWND` / `SurfaceTargetFromMetalLayer` /
`SurfaceTargetFromAndroidNativeWindow`），但 Gox 平台后端（x11/win32/cocoa）
没有向 gfx 层暴露原生窗口句柄，`gpu.Surface` 也没有 `Present()` 实现。
**「三平台真机验证」之前必须先补两件事**：

1. 平台 Surface 增加原生句柄暴露（x11: Display+Window；win32: HWND；
   cocoa: CAMetalLayer）—— 建议走「可选接口」而非扩 Surface 契约；
2. `backend_wgpu.go` 用 `wgpu.NewSurface` + `Configure` 替换离屏
   `target` 纹理，ShowRegions 收尾改为 surface.Present。

在此之前，A 路径的「默认化」只能做到离屏验证级别，数字均为兜底口径。

## wgpu 交叉编译矩阵抽查（`--xcompile`，linux/amd64 host）

| 目标 | 结果 |
|---|---|
| linux/amd64（host） | ok |
| linux/arm64 | ok |
| windows/amd64 | ok |
| darwin/arm64 | **FAIL**：goffi/purego 的 fakecgo `_cgo_init` 符号冲突，
darwin 目标需要本机 cgo 工具链（`CGO_ENABLED=1` + Apple 工具链），
「单机交叉出 mac 包」在 `-tags gpu` 下不成立 |
| js/wasm | **FAIL**：`wgpu/hal/allbackends` 无 wasm 后端文件，GPU 轨不覆盖 wasm |

修正 `gfx/gpu/doc.go` 的口径：wgpu 轨「交叉编译 ✅」仅对 windows/linux
成立；darwin 与 wasm 需要 cgo / 不支持。release.yml 的 mac 构建保持
默认（nogpu）不受影响。

## CI 接入方案（已落地）

`.github/workflows/gpu-present.yml`：

- `no-hardware` job（ubuntu-latest + Xvfb）：`go test -tags gpu ./gfx/gpu`
  （含像素级 A/B）+ 三轨核销表 + 4K 基准，产物上传 7 天，全程标注兜底口径；
- `hardware` job（`[self-hosted, gpu-hardware]` 占位）：无 runner 时自动
  skip 不报红；硬件 runner 挂上后零改动即为可信口径，产物留 90 天。

## 本机实测对照数字（Linux x86_64 32 核沙箱，无硬件 —— **兜底口径，不进闸门、不对外**）

`scripts/bench-present-4k.sh`，4K 全帧，cpu-ns/op（应用进程 rusage）：

| 口径 | 数字 | 说明 |
|---|---|---|
| CPU_X11（全帧） | 39.1 ms | x11 逐区域 PutImage，应用 CPU |
| CPU_X11（512² 脏区） | 1.04 ms | 脏区优化在 CPU 侧真实兑现 |
| GPU_AppCPU（全帧） | 175.1 ms | 软件后端指纹：与 Total 差 <1% |
| GPU_Total（全帧） | 181.0 ms | ≈ CPU 的 4.6x —— 与「慢约 5x」口径一致 |
| GPU_Dirty（512²） | 147.9 ms | 与全帧几乎同速：软件 HAL 每 Draw 全量重建，脏区不省 |
| GPU_UploadOnly | 2.68 ms | **唯一可迁移到真实硬件的数字**；硬件上应用 CPU 收益 ≈ 39.1 → 2.7ms（~14x） |

## 硬件覆盖待办（G-1 收尾条件）

- [ ] 在带 GPU 硬件的机器（win32 / x11 / cocoa 各一）重跑
      `scripts/gpu-track-audit.sh` 与 `bench-present-4k.sh`，覆盖本表
      「有硬件路径」列，出 CPU blit vs GPU 合成同口径对照数字；
- [ ] swapchain 接入完成后，在真机验证窗口化上屏（非 Readback）；
- [ ] 移动端接入决策：android 有 `SurfaceTargetFromAndroidNativeWindow`，
      可作 G-3 阶段项评估。
