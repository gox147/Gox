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
   方案，见下文更新）；cocoa 未实现
   （需 CAMetalLayer，setWantsLayer 方案未验证）。
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
  cocoa ❌（CAMetalLayer 未实现，仍待 G-3）。
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
