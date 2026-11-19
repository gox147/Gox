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
| **窗口化上屏最后一跳** | ✅ **已接 swapchain（2026-11-17）**：`backend_wgpu.go`
ShowRegions 收尾走 `wgpu.Surface.Present`（acquire 交换链纹理 → 渲染 →
Present），离屏 target 纹理仅作测试 Readback/降级兜底。运行期任何
acquire/present 错误 → 永久退回内层 CPU blit（`swapBroken` 降级语义，
测试钉住：`gfx/gpu/swapchain_gate_test.go`） |
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

### 2026-11-17 更新：两件硬前提已落地（代码层），真机验证待做

1. ✅ 原生句柄暴露：新增可选接口 `gfx.NativeWindowProvider`
   （`gfx/native_window.go`，不扩 Surface 契约）。win32 已实现
   （HINSTANCE+HWND）；x11 已实现（2026-11-18 补 Xlib Display* dlopen
   方案，见下文更新）；cocoa 已实现（2026-11-19 两段式 CAMetalLayer，
   见下文更新）。
2. ✅ swapchain 上屏：`backend_wgpu.go` 接入
   `Instance.CreateSurfaceUnsafe` + `Surface.Configure/GetCurrentTexture/
   Present`，支持 resize 重配、非 RGBA8 格式备用管线、运行期失效永久
   降级 CPU blit。包装点（`cmd/gox/gpu_present_init.go`）新判据：
   `PresentsToWindow()` 为 false 时**不包装真窗口**（旧行为会黑窗）。

**诚实口径**：swapchain 路径在本沙箱（无 GPU 硬件、无真平台窗口）只能
验证 gate 降级语义与像素 A/B 零退化（软件适配器兜底口径），真机窗口化
上屏与性能数字属于上面「硬件覆盖待办」，未做不得声称 A 路径完成。

### 2026-11-18 更新：x11 Xlib Display* dlopen 方案落地（硬前提补齐）

- ✅ `gfx/x11/xlib.go`：purego `Dlopen("libX11.so.6")` → `XOpenDisplay(NULL)`
  取 Xlib Display*，**零 cgo**（purego 已在依赖中，gfx/cocoa 同款），
  不破坏「零 cgo、单机交叉编译」发行根基。要点：与 xgb 同源（都读
  DISPLAY，连同一 server，Window id 可跨连接引用）；进程内共享一条
  连接、从不 XCloseDisplay；启动即调 XInitThreads；dlopen/连接任一
  失败安静返回 0 且不重试（sync.Once），GPU 侧 gate 拒绝建链退
  CPU blit。降级语义有测试钉住（`gfx/x11/xlib_test.go`，无头沙箱
  PASS：无 X server 时 Display==0，行为与无 GPU 轨一致）。
- 至此三平台句柄暴露的**代码前提**全部就位：win32 ✅ / x11 ✅（本轮）/
  cocoa ✅（2026-11-19 两段式，见下文更新）。
- 诚实口径：x11 真机点亮（swapchain 真上屏 + 性能数字）属于「硬件
  覆盖待办」，本更新只核销代码层缺口，不构成 A 路径完成。

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

## 复跑核销（2026-11-15，Linux x86_64 32 核沙箱，无硬件）

`gpu-track-audit.sh` 全量复跑：三轨输出与 2026-11-08 基线逐项一致
（nogpu=none / wgpu=software 兜底口径 / cgo=none），接入面结论不变
（移动端未接入、swapchain 接入代码已落地待真机验证、CI 跑批文件在位）。
`go test ./gfx/gpu` 与 `go test -tags gpu ./gfx/gpu` 均绿。
`--xcompile` 矩阵复跑：windows/amd64、linux/arm64 ok；darwin/arm64
fakecgo 符号冲突、js/wasm 构建约束排除，均与上表一致。

CI 首跑验证：gpu-present.yml 入仓后从未被触发（path 过滤未命中），
2026-11-15 以 workflow_dispatch 强制首跑。首跑踩出一个 CI 缺陷：
hardware job 只有 runs-on 标签守卫，无 runner 时停在 queued 直到
timeout 报红（注释声称「自动 skip」但 yaml 未实现）。已修复（f6c64d3）：
`if: vars.GPU_HARDWARE_RUNNER == 'true'` 仓库变量守卫。修复后 push 触发
复跑：兜底口径 job success（test + 核销表 + 4K 基准，产物上传），
hardware job skipped —— 「CI gpu 跑批接入」验收项闭环。挂硬件 runner 后
置仓库变量 `GPU_HARDWARE_RUNNER=true` 即启用硬件口径 job。

## 硬件覆盖待办（G-1 收尾条件）

- [ ] 在带 GPU 硬件的机器（win32 / x11 / cocoa 各一）重跑
      `scripts/gpu-track-audit.sh` 与 `bench-present-4k.sh`，覆盖本表
      「有硬件路径」列，出 CPU blit vs GPU 合成同口径对照数字；
- [ ] swapchain 接入完成后，在真机验证窗口化上屏（非 Readback）；
- [ ] 移动端接入决策：android 有 `SurfaceTargetFromAndroidNativeWindow`，
      可作 G-3 阶段项评估。

## 复跑核销（2026-11-18 04:01，Linux x86_64 32 核沙箱，无硬件 —— 兜底口径）

`gpu-track-audit.sh` 全量复跑：三轨与 11-08/11-15 基线逐项一致
（nogpu=none / wgpu=software 兜底口径 / cgo=none 骨架）；接入面不变。
`go test ./gfx/gpu` 与 `go test -tags gpu ./gfx/gpu` 双轨全绿，
`go test ./gfx/...` 全包无失败。本节数字维持兜底口径纪律：不进闸门、不对外。

## 移动端接入决策（2026-11-18，G-1 验收项闭环）

**决策：android / ios / harmony 现阶段不接入 SetPresentWrapper**，理由三条：

1. **架构事实**：移动端上屏由宿主完成，不是 Go 侧窗口 —— android 走
   direct ByteBuffer + Kotlin `flush`（`Bitmap.copyPixelsFromBuffer`
   + invalidate），ios 走宿主 malloc 缓冲 + Swift 拷出 CGImage，harmony
   同构。`SetPresentWrapper` 的 swapchain 语义需要"Go 进程持有可建
   surface 的原生窗口"，移动端通道层根本不提供这个对象。
2. **改造成本 vs 收益**：接入需把宿主上屏从 Bitmap/CGImage 拷贝改造成
   SurfaceView/ANativeWindow 或 CAMetalLayer 直通（Kotlin/Swift 双侧
   契约改动 + 三处同步纪律），收益仅省宿主侧"一次额外拷贝" —— 该拷贝
   只占移动端帧预算的小头，且移动端 GPU 轨在 wgpu 交叉编译矩阵里本就
   未覆盖（android 目标需另行验证 wgpu GLES 后端）。
3. **优先级口径**：C 路径拍板 A 路径先行，A 路径验收是三桌面平台
   真机点亮；移动端属 G-3 之后可选阶段项，现在动手只会摊薄验证带宽。

条件重启：G-3 三桌面平台真机点亮后，若移动端实测证明宿主拷贝是帧率
瓶颈，再以 `SurfaceTargetFromAndroidNativeWindow`（android）/
`SurfaceTargetFromMetalLayer`（ios）立项，独立验收。

## cocoa 设计约束（2026-11-18，G-3 实现前置输入）

cocoa 的 CPU blit 上屏是 `CALayer.contents = NSImage`（`gfx/cocoa`
cocoa.go Show 路径：`s.layer.Send(selSetContents, s.nsimg)`）。这与
win32/x11 有本质差别：**那两家的句柄是纯只读探针，cocoa 的 CAMetalLayer
获取必然要改写 view 的 layer**。若在 `NativeWindowTarget()` 里直接
`setWantsLayer + setLayer(CAMetalLayer)`，而 GPU 建链失败（Metal 不可用/
wgpu 初始化失败）回退 CPU blit 时，`contents` 路径在 CAMetalLayer 上
不渲染 —— 黑窗，且违反「降级路径窗口照常开」的铁律。

**实现约束（G-3 cocoa 项验收时逐条检查）**：

- 切层必须是**可回滚的两段式**：先探测（GPU 链能在不切层的前提下验证
  Metal 适配器可用），探测通过才 `setLayer(CAMetalLayer)`；探测失败
  零副作用，CPU blit 照常。
- 切层成功后 `allocBackbuffer` 的 `contentsScale` / `nsimg` 路径要分流：
  Metal 层不再走 NSImage contents，Resize/跨屏（`setScale`）时两层路径
  都要保鲜（cocoa 是唯一缓存 scale 的后端，见 window.go 注释）。
- 降级测试钉住：强制 Metal 探测失败时，cocoa 窗口 CPU 上屏行为与
  未引入 GPU 轨前逐像素一致（26 组件快照口径）。

在满足上述约束前，cocoa 的 NativeWindowProvider 维持未实现状态 ——
宁可缺一个平台，不做降级会黑窗的实现。

### 2026-11-19 更新：cocoa 两段式按上述约束落地（`gfx/cocoa/metal.go`）

约束第 1 条与第 3 条本轮闭环，第 2 条（Metal 层下 Resize/跨屏的
contentsScale 分流）**维持待办** —— 它只在切层成功后才可达，属于真机
覆盖项的一部分，沙箱验不到，不预写未经真机验证的代码：

- ✅ 两段式落地：第 1 段 `NativeWindowTarget()` 只**创建** CAMetalLayer
  （`alloc+init`，设 contentsScale），不碰 view —— 建链失败零副作用；
  第 2 段新增可选接口 `gfx.NativeWindowCommit.CommitNativeWindow(ok)`，
  由 `backend_wgpu.go` 在 `CreateSurfaceUnsafe + Configure` **成功后**
  才发 `true`（`setLayer + setWantsLayer`，layer-hosting），建链失败
  或 swapchain 运行期失效（`swapBroken`）发 `false` —— 恢复原
  backing layer，CPU blit（contents=NSImage）立即可用，窗口不黑。
  `false` 幂等（未提交时 no-op），降级路径每帧触达不重复恢复。
- ✅ swapchain 接线：`initSwapchainTry` 增加 `case "cocoa"` →
  `SurfaceTargetFromMetalLayer`（wgpu 原生支持，`Display` 不用）。
- ✅ 降级测试钉住（`gfx/gpu/commit_gate_test.go`）：建链失败恰好一次
  `false` 且不得误发 `true`；`degradeSwapchain` 每次失效触达 `false`；
  降级后渲染照常委托 inner。无头沙箱（无 Metal）PASS。
- ✅ 不破坏发行链：`GOOS=darwin CGO_ENABLED=0 go build ./gfx/...` 通过
  （purego objc，零 cgo）；linux 默认轨与 win32 交叉编译通过；26 组件
  CPU vs GPU 双轨快照 A/B（阈值 0）全绿，Prism 视觉一致性零退化。
- 诚实口径：真机点亮（Metal 适配器探测、切层后的真上屏、Resize/跨屏
  保鲜、性能数字）属于「硬件覆盖待办」，未做不得声称 cocoa 平台 GPU
  上屏可用。

## G-4 视觉闸门第一块落地：26 组件 CPU vs GPU 双轨快照 A/B（2026-11-18）

`gfx/gpu/snapshot_ab_test.go`（`-tags gpu`），对应 G-4 验收的
「26 组件快照像素级 A/B + 无硬件跳过逻辑钉住」两件：

- **表驱动双轨对照**：Prism §3 S3~S7 的 26 个画廊脚本
  （testdata/shots/*.js）逐个挂两遍 —— CPU 参照帧（captureSurface）与
  GPU 合成帧（gpu.Surface + Readback）—— 逐像素比对，**阈值 0**。
  文本/形状/抗锯齿出自同一条 CPU 光栅化链，GPU 只做「帧缓冲 → 合成
  quad」，差异只能来自 GPU 合成，不存在「可接受的近似」。
- **非空白守卫**：比对前先断言 CPU 参照帧非空（颜色数 ≥8 / 墨量 ≥400，
  与画廊判据同源）—— 两张白纸的 0 差异没有意义。
- **入表纪律（可复现性）**：只收静态脚本。menu 预展开、tree/datepicker
  收起态；**progress.js 不入表** —— 其 indeterminate 色块是 1200ms 循环
  动画（16ms 心跳按 wall-clock 推进），双轨两次 mount 取帧相位不可对齐
  （实测同机双跑 4~14 个 accent 色差异像素，全因相位，不是合成 bug）。
  tooltip/dialog/drawer/spinner/skeleton/datav-* 同理不入表。
- **跳过逻辑有测试钉住**（G-4 验收原文要求）：唯一 skip 决策点是
  `snapshotABGate`，由 `TestGPUSnapshotABGate` 钉住三条语义 ——
  无适配器必 skip；**软件适配器必须照跑**（它是 CI 确定性后端，视觉
  闸门要像素一致不要帧率，不许拿「没显卡」当跳过理由）；硬件适配器跑。
- **CI**：`.github/workflows/gpu-present.yml` no-hardware job 新增显式
  步骤（`go test -tags gpu ./gfx/gpu -run TestGPUSnapshotAB -v`），产物
  `gpu-snapshot-ab.txt` 单列 —— 供「双轨对照真跑过一次」留证（兜底
  口径：跑在 wgpu-software 上，钉像素回归，不钉性能）。
- 局部帧（脏矩形）分支由既有 `TestGPUPresentPixelExact`（悬停交互）覆盖，
  本套件钉整帧分支；两者合起来是完整的双轨视觉门禁。

实测（2026-11-18，Linux x86_64 32 核沙箱，无硬件 —— wgpu-software 兜底
口径）：26/26 子用例差异像素 **0**，套件总耗时 ~0.25s，连跑 5 次 +
`./gfx/...` 全包回归零失败。帧率/上屏耗时基准进 CI（与 T3-2 共用口径）
与「无硬件 runner 跳过性能断言」属 G-4 后续项，本次未动。

## 复跑核销（2026-11-18 08:00 执行窗口，Linux x86_64 32 核沙箱，无硬件 —— 兜底口径）

G-1 现状核销的例行复跑，三轨与 11-08/11-15/11-18(04:01) 基线逐项一致：

```text
TRACK=nogpu kind=none backend=none available=false          （恒 ErrNotBuilt）
TRACK=wgpu kind=software backend=wgpu-software available=true ← 兜底口径
TRACK=cgo kind=none backend=cgo available=false             （骨架纪律钉住）
```

接入面不变：SetPresentWrapper 仅桌面接入；移动端决策不接入；swapchain
Present 路径在位（真机验证待做）；CI gpu 跑批文件在位（gpu-present.yml）。

回归（同环境同口径）：`go test ./gfx/gpu`（nogpu 轨）、
`go test -tags gpu ./gfx/gpu`（wgpu 轨）、`go test ./gfx/...` 全包均绿；
26 组件 CPU vs GPU 双轨快照 A/B（`TestGPUSnapshotAB26`）PASS，
差异像素阈值 0，耗时 0.24s —— Prism 视觉一致性维持零退化。
本节数字为无硬件兜底口径：不进闸门、不对外；硬件口径仍属
「硬件覆盖待办」，未在真机覆盖前不得声称 A 路径完成。

## 交叉编译矩阵修复：gfx/x11 GPU 轨 purego→goffi（2026-11-19 04:10 执行窗口，Linux x86_64 32 核沙箱，无硬件 —— 兜底口径）

**发现（真实缺陷）**：真零 cgo 口径（`CGO_ENABLED=0`）下 `-tags gpu` 交叉编译矩阵
只有 windows/amd64 能过 —— linux/amd64、linux/arm64 链接 `cmd/gox` 即炸：
`link: duplicated definition of symbol _cgo_init, from goffi/internal/fakecgo
and purego/internal/fakecgo`。此前的「win/linux ok」基线是在宿主 CGO 默认开启
（gcc 在位）下测得，掩盖了零 cgo 发行约束下的真实状况 —— 审计脚本的
`--xcompile` 矩阵本窗口起已加 `CGO_ENABLED=0` 对齐口径。

**根因**：`gfx/x11/xlib.go`（2026-11-18 purego dlopen 方案）是 linux 下
`-tags gpu` 构建中唯一的 purego 入口，与 go-webgpu/goffi 各带一份 fakecgo，
同时入链即冲突 —— 正是 gfx/softmedia/pulse_disabled.go 早已写进文档的坑，
xlib.go 恰恰踩中自家纪律的例外。

**修复**：GPU 轨下 xlib 换用 goffi 的 `LoadLibrary/GetSymbol/CallFunction`
（与 go-webgpu 同源 fakecgo，天然不冲突）：
- `xlib_gpu.go`（`linux && gpu && amd64||arm64`）：goffi dlopen 版，降级语义与
  purego 版逐字一致（失败安静退 0、sync.Once 不重试、GOX_XLIB_LIBRARY 覆盖）；
- `xlib_gpu_stub.go`（`linux && gpu && 其余架构`）：诚实交 0，goffi dlopen 本就
  只覆盖 amd64/arm64；
- `xlib.go` 改 `linux && !gpu`：默认构建维持 purego 版不动，零行为变化。

**修复后矩阵（CGO_ENABLED=0，-tags gpu，cmd/gox）**：windows/amd64 ✅、
linux/amd64 ✅、linux/arm64 ✅、darwin/arm64 ❌（冲突源在 gfx/cocoa 的 purego
objc_msgSend，与 x11 无关，属已知 darwin 冲突，需 goffi 化或上游解决，另案）。

**功能验证**：Xvfb 真实 X server 下 `go test -tags gpu ./gfx/x11 -run
TestXlibDisplaySilentDegrade` 拿到真实 Display*（goffi 路径全链路走通，
非仅降级）；无 X 环境降级路径 PASS。`go test ./gfx/gpu`、
`go test -tags gpu ./gfx/gpu`、`go test ./gfx/...` 全绿；
`TestGPUSnapshotAB26` PASS（阈值 0）—— Prism 视觉一致性未受触碰。

**诚实口径**：本节全部为无硬件兜底口径；x11 Display* 真实点亮 swapchain 仍属
G-3 三平台真机验证待办。darwin 交叉编译矩阵修复不在本窗口范围。

## x11 swapchain 端到端点亮 + BGRA 格式修复（2026-11-19 06:00 执行窗口，Linux x86_64 32 核沙箱，Xvfb + wgpu-software —— 兜底口径）

本窗口把「swapchain 真机点亮」待办里**不依赖 GPU 硬件**的部分拆出来做掉了：
wgpu 软件后端在 Linux 上原生支持 Xlib window target（`hal/software/blit_linux.go`，
XPutImage 经 goffi dlopen libX11），**有 X server（Xvfb 即可）就能真点亮**。

### 1. 新测试：TestSwapchainX11WindowE2E（gfx/gpu/swapchain_x11_e2e_test.go，-tags gpu && linux）

真平台窗口（gfx/x11 工厂建真窗）→ gpu.New 建链 → Show 四象限纯色帧 →
**XGetImage 从窗口读回**逐像素比对（按 XImage 自带 masks 解码，不硬编码字节序）。
此前两层验证（gate 语义伪句柄 + 离屏 Readback）之间的接缝层首次被钉住。

### 2. 抓到并修复一个真实缺陷：窗口化路径红蓝互换（backend_wgpu.go）

旧 `initSwapchainTry` 优先选 RGBA8Unorm（为了与离屏管线同格式免建第二条管线）；
但软件适配器的表面格式列表 **BGRA8Unorm 在前**，且其 XPutImage blit **不做任何
通道转换**（framebuffer 存 [B,G,R,A]）—— 强选 RGBA8 导致红蓝互换直接上屏。
Xvfb 下逐像素实测复现（红象限读回为蓝）。修复：窗口化 swapchain 优先 BGRA8Unorm
（三平台原生表面首选格式均为 BGRA：X11 TrueColor / DXGI / CAMetalLayer），拿不到
再回退首选格式；离屏路径不动。**这也是此前「真机点亮」被整体推迟的隐性风险首次
被无硬件手段抓到。**

### 3. 发现并钉住一个上游缺陷：goffi libX11 装载 × wgpu-software 同进程段错误

实测复现矩阵（Linux x86_64，32 核沙箱）：
- TestSwapchainX11WindowE2E **单独进程** 连跑 5 次全绿（含真建链 + 窗口像素逐点一致）；
- 与其它 gfx/gpu 测试同进程时，后续测试必段错误 —— 且污染在进程级：
  哪怕 e2e 只走到「goffi LoadLibrary libX11 + 一次失败的 XOpenDisplay」（X server
  不可达场景），后续 wgpu-software 测试（快照 A/B 等）也崩；崩溃无 Go 栈
  （gdb core 显示 PC 在未映射内存，调用链经 runtime/cgo），带 6~8 个 OS 线程；
  二分排除了测试顺序、BGRA 改动、gfx/x11 链接本身三个假设。
- 判定：**上游 goffi FFI × wgpu-software 交互缺陷**，本仓不可修。CI 上 2026-11-19
  04:33 记录的 checkbox 偶发段错误（run 44391316067）与本现象同签名（wgpu-software
  段错误、无栈），建议同案排查。

### 4. 测试的进程隔离闸门（不是裸 if 的口径）

`TestSwapchainX11WindowE2E` 要求 `GOX_GPU_X11_E2E=1` 才运行；CI 用**独立进程**
专跑（`GOX_GPU_X11_E2E=1 xvfb-run -a go test -run TestSwapchainX11WindowE2E`），
其余套件进程里恒 skip。三态验证（无 X 全量 / Xvfb 全量 / Xvfb 独立进程 e2e）均
多次连跑全绿。

### 5. 口径纪律

本节全部为**兜底口径**：软件适配器上钉住的是「接线正确性 + 像素零退化」，
不产生任何可对外引用的性能数字；硬件口径仍归 hardware job（GPU_HARDWARE_RUNNER）。
真机三平台点亮（win32 HWND / cocoa CAMetalLayer 侧）仍属 G-3 硬件覆盖待办；
x11 这一行自此从「纯真机待办」中拆出（接线 + 像素已钉，性能待硬件）。

## darwin 交叉编译冲突修复：nofakecgo 官方开关（2026-11-19 08:00 执行窗口，Linux x86_64 32 核沙箱，编译期验证）

上一窗口遗留的「另案」——darwin GPU 轨 `CGO_ENABLED=0` 交叉编译炸链接——本窗口修复，
A 路径「零 cgo、单机交叉编译」矩阵自此五平台全通（js/wasm 维持已知不支持，诚实输出）。

### 根因与修复

- **根因**：darwin GPU 轨构建里，gfx/cocoa（purego/objc，默认轨必需）与 goffi（GPU 轨
  wgpu 调用）**各带一份 `internal/fakecgo`**，两者都注入 `_cgo_init` 等符号；
  `CGO_ENABLED=0` 下链接 `cmd/gox` 报
  `duplicated definition of symbol _cgo_init ... goffi/internal/fakecgo and purego/internal/fakecgo`。
- **修复**：goffi 自带官方开关——`ffi/fakecgo_unix.go` 的构建约束是
  `(linux || darwin || freebsd) && !cgo && !nofakecgo`，注释明确写着「for projects that
  already provide these symbols through another package (e.g., purego's internal/fakecgo)」。
  darwin GPU 构建加 `nofakecgo` 即让 goffi 复用 purego 的 fakecgo（两者同源 fork，符号集兼容）。
- **实测**（编译期，`go build -tags gpu,nofakecgo`）：darwin amd64 ✅ / darwin arm64 ✅。

### 平台差异化口径（重要，别抄错）

| 平台 | GPU 轨构建 tag | 原因 |
|---|---|---|
| darwin (amd64/arm64) | `-tags gpu,nofakecgo` | cocoa 的 purego 提供 fakecgo，必须禁 goffi 自带份 |
| linux (amd64/arm64) | `-tags gpu`（**不能**加 nofakecgo） | GPU 轨已无 purego 用户，禁掉 goffi fakecgo 后无人提供 `runtime.iscgo`，链接报 `relocation target crosscall2 not defined`（实测复现） |
| windows/amd64 | `-tags gpu` | goffi fakecgo 仅 unix 生效，无冲突 |
| js/wasm | 已知不支持 | wgpu hal/allbackends 无 wasm 构建 |

落地：`scripts/gpu-track-audit.sh` 的 `--xcompile` 矩阵按平台自动选 tag（darwin 两架构
加入矩阵），复跑输出 win/linux amd64+arm64 与 darwin amd64+arm64 全 `ok`。

### 诚实口径

- 本节全部为**编译期验证**（Linux x86_64 沙箱交叉编译），无 GPU 硬件，不产生任何性能
  数字；darwin 真机上 goffi×purego 混用 fakecgo 的**运行时语义**（回调、线程、异常路径）
  仍属 G-3 cocoa 真机点亮验证项，不得因编译通过声称 cocoa GPU 轨可用。
- 本窗口未触碰任何 Go 代码（仅 shell 脚本 + 文档），26 组件 CPU vs GPU 双轨快照 A/B
  （`-tags gpu`，软件适配器兜底口径）复跑 PASS，差异像素 0 —— Prism 视觉一致性零退化。

## 2026-11-19 22:00 执行窗口复跑核销（Linux x86_64 32 核沙箱，无 GPU 硬件，全部为兜底口径：不进闸门、不对外）

### 环境与基线

- 基线 commit 778392a（main，CI gpu-present run 44523852777 success）；
  本窗口纯文档 + 审计脚本改动，不触碰任何 Go 渲染代码。

### 三轨复跑（gpu-track-audit.sh）

| 轨 | BackendKind | Backend | 结论 |
|---|---|---|---|
| nogpu | none | none | 恒 ErrNotBuilt，CPU blit |
| wgpu | software | wgpu-software | 无硬件兜底口径：不进闸门、不对外 |
| cgo | none | cgo | 骨架阶段恒 ErrNotBuilt |

与 11-08 / 11-15 / 11-18（04:01、08:00）基线逐项一致，无漂移。
回归：`go test ./gfx/gpu`（nogpu 轨）与 `go test -tags gpu ./gfx/gpu`（wgpu 轨）均绿；
`go test ./gfx/...` 全包零失败。

### 本窗口修复：交叉编译矩阵抽查补齐 windows/arm64（口径对齐）

- 发现：be1671a 实测口径为「win/linux amd64+arm64 ✅」共 6 个可交叉目标，但
  `gpu-track-audit.sh --xcompile` 矩阵抽查只列了 `windows/amd64`，windows/arm64
  从未被脚本覆盖；而上一节文档却声称「win/linux amd64+arm64 全 ok」—— 文档口径
  超出脚本实际验证面。本窗口补齐：矩阵加入 `windows/arm64`（`-tags gpu`），
  实测编译通过，脚本输出自此与文档声明逐目标对齐。
- 复跑矩阵（CGO_ENABLED=0）：windows/amd64 ✅ windows/arm64 ✅ darwin/amd64 ✅
  （-tags gpu,nofakecgo）darwin/arm64 ✅（同前）linux/amd64 ✅ linux/arm64 ✅；
  js/wasm 维持已知不支持（hal/allbackends 无 wasm 构建，诚实输出）。
- 同窗口订正 `.github/workflows/gpu-present.yml` 头部注释：旧「labels 不匹配时
  自动 skip」口径已被 2026-11-15 首跑实测证伪（无 [gpu-hardware] runner 时 job
  停 queued 至 timeout 报红），订正为 GPU_HARDWARE_RUNNER 仓库变量守卫口径，
  与 f6c64d3 的实际实现及 job 级注释一致。

### 口径纪律维持

本窗口全部数字为无硬件兜底口径，不进闸门、不对外，不构成硬件成果。
G-1 收尾仍差硬件覆盖三项（win32/x11/cocoa 三平台真机 audit+bench、swapchain 真机
点亮对照数字），需真机，不标 done。26 组件 CPU vs GPU 双轨快照 A/B（CI run
44523852777）差异像素 0 —— Prism 视觉一致性维持零退化，本窗口变更未触碰视觉面。

## 2026-11-20 00:05 执行窗口复跑核销（Linux x86_64 32 核沙箱，无 GPU 硬件，全部为兜底口径：不进闸门、不对外）

### 环境与基线

- 基线 commit 751687e（main，本轮 builtins 侧新提交后的首次 GPU 三轨复跑；
  main 自 778392a 起漂移，需确认渲染面无回归）。
- 本窗口为纯核销运行，不触碰任何 Go 渲染代码，无代码变更提交。

### 三轨复跑（gpu-track-audit.sh）

| 轨 | BackendKind | Backend | 结论 |
|---|---|---|---|
| nogpu | none | none | 恒 ErrNotBuilt，CPU blit |
| wgpu | software | wgpu-software | 无硬件兜底口径：不进闸门、不对外 |
| cgo | none | cgo | 骨架阶段恒 ErrNotBuilt |

与 11-08 / 11-15 / 11-18 / 11-19 基线逐项一致，无漂移。接入面核销不变：
swapchain 已接线（真机验证待做）、移动端未接入、CI gpu 跑批在位。

### 回归与交叉编译矩阵

- `go test ./gfx/gpu`（nogpu 轨）与 `go test -tags gpu ./gfx/gpu`（wgpu 轨）均绿；
  `go test ./gfx/...` 全包零失败。
- `--xcompile` 矩阵（CGO_ENABLED=0）：windows/amd64+arm64、linux/amd64+arm64
  （`-tags gpu`）、darwin/amd64+arm64（`-tags gpu,nofakecgo`）共 6 目标全 `ok`；
  js/wasm 维持已知不支持（wgpu hal/allbackends 无 wasm 构建），诚实输出。

### 视觉与降级闸门

- `TestGPUSnapshotAB26`（26 组件 CPU vs GPU 双轨快照，阈值 0）PASS —— Prism
  视觉一致性零退化；`TestGPUPresentPixelExact` PASS；`TestSwapchainGate`
  五道降级 gate 全绿。

### 诚实口径

本窗口全部数字为无硬件兜底口径，不进闸门、不对外，不构成硬件成果。
G-1 收尾仍差硬件覆盖三项（win32/x11/cocoa 三平台真机 audit+bench、swapchain
真机点亮对照数字），需真机，不标 done。
