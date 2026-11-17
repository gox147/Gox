# GPU 光栅化可行性探针（G-2 · 离线部分）

生成：`gfx/probe_glyphatlas_test.go`（字形图集内存）、`gfx/probe_drawcensus_test.go`（绘制普查）、
`gfx/probe_gpuraster_test.go`（SDF 像素 A/B，硬件侧）。
普查钩子：`gfx/census.go`（默认 nil，no-op 内联，默认构建零行为变化——回归测试全绿钉住）。

日期：2026-11-17 ｜ 环境：Linux x86_64 沙箱，**无 GPU 硬件**（wgpu-software 兜底口径），
32 核，Go 1.26 工具链，字体 DejaVu/Noto（CJK 覆盖 100%）。

## 口径纪律（先行声明）

- 字形图集与绘制普查的数字**与 GPU 硬件无关**（输入就是现有 CPU 光栅化产物/调用形态），可进结论。
- SDF 像素 A/B 只能在 hardware 适配器上跑（原因见 §4，software 后端不可作正确性参照），
  本机仅产出探针代码与跳过逻辑，**不出数字**。
- 性能数字一律不进闸门、不对外，直到 gpu-hardware runner 复测。

## 1. 字形图集内存测算（离线，结论可进闸门）

`TestProbeGlyphAtlasMemory`，实测输出（本机）：

| 字号 | 可用字形 | 位图合计 | 2048² R8 图集 | 全块光栅化耗时 |
|---|---|---|---|---|
| 14 | 21,360 | 4.45 MiB | 2 页 (8 MiB) | 140ms |
| 16 | 21,360 | 5.69 MiB | 2 页 | 144ms |
| 20 | 21,360 | 8.26 MiB | 3 页 | 157ms |
| 28 | 21,360 | 15.49 MiB | 4 页 | 173ms |

- fallback 链覆盖：URO 主块 21,316/20,992 (99.5%)+，ASCII/CJK 标点/全角齐全，可用字形 21,360。
- **全字号全块图集 ≈ 11 页 / 44 MiB**（R8），单字号 2-4 页；size=16 全块建图仅 144ms ——
  **全量图集 + 按字号惰性建页完全可行**，CJK 不构成内存墙。
- 真正的坑是**失效策略**：现有 `glyphLRU` 容量 1024 条（`gfx/font.go`），全块×4 字号 =
  85,440 条（**超容 83×**）；中文一屏典型驻留（500 字 × 2 字号 ≈ 1000 条）**正好压在容量线上**
  —— GPU 文本阶段必须把 LRU 换成显式图集页管理（按页失效、按字号分页），不能沿用现有 LRU。
- 合成粗体复用同一掩码（压两遍 blitMask），**不增加图集内存**；真粗体面（若启用）按独立
  掩码计，图集随真粗体面数量线性增长。

## 2. 绘制普查：FillRect/Draw 调用形态（离线，结论可进闸门）

`TestProbeDrawCensus`，稳定态单帧（480×360，挂载真实 UI 后手动驱动一次 `Draw`）：

| 场景 | fillrect | glyph | aapixel | 过绘因子 | 颜色种类 | fillrect 同色段切换（理论最小 draw call） |
|---|---|---|---|---|---|---|
| 表单页 | 9 | 57 | 14,478 | 0.21× | 9 | 5 |
| 数据面板 | 8 | 92 | 22,816 | 0.23× | 4 | 1 |

三个改变设计前提的事实：

1. **矩形不是主导项**：Prism 全组件圆角化后，单帧 FillRect 只剩个位数；调用数 99.6% 是
   `blendPixel` 散点（圆角 SDF 覆盖率，decor.go `fillRoundRect` 角带逐像素）。
2. **合批潜力极好**：颜色种类 ≤9，fillrect 按颜色排序后同色段切换 ≤5 —— 矩形阶段一个
   instanced draw（每实例携带 color）即可覆盖，管线切换次数趋近于零。
3. **第一阶段必须是 SDF 圆角矩形，不是纯色 quad**：只做纯 quad 的话，26 组件快照在所有
   圆角边缘必然翻车（视觉门禁不可过）。正确姿势：每 quad 携带 rect+color+rad，
   fragment shader 复刻 `roundCov` 同一个盒形 SDF 公式（公式已写入
   `probe_gpuraster_test.go` 的 probeWGSL，等硬件验证）。
   - 渐变（fillRoundGradient）与阴影模糊是逐像素大户，属后续阶段（G-3 的图片/路径阶段）。

## 3. 体积与依赖（沿用 11-08 核销，本机复验构建不变）

- wgpu 轨体积 +6.16MB（29.06→35.21，gpu-track-audit.md 既有实测）；交叉编译矩阵不变
  （win/linux 可交叉，darwin 需本机 cgo，wasm 无 GPU 轨）。本次未改动构建面。

## 4. SDF 像素 A/B：**software 后端不可作正确性参照（本次最重要的新发现）**

`probe_gpuraster_test.go` 按设计在 hardware 适配器上对拍 CPU `fillRoundRect`（同公式、
同 blend 语义、9 个 quad 单 draw call 合批、edge case 含 rad=0/1px/半透明/越界）。

本机（无硬件）实测发现，wgpu **software 后端根本不是忠实的光栅化器**，而是三段特化路径：

| 输入形态 | software 后端实际行为 |
|---|---|
| 全屏 quad | `tryFullscreenBlit` memcpy 快路径（#241），无纹理绑定时输出垃圾 |
| FS 无 @location 输入 | **不执行 shader**，`resolveFragmentColor` 兜底（读 uniform，否则白 {1,1,1,1}） |
| FS 有 varying | SPIR-V 解释器逐像素执行，但顶点属性搬运在探针布局下**结果错误**（纯红 varying 实测输出 {0,1,152}） |

现有 `TestGPUPresentPixelExact` 之所以绿，是因为呈现路径恰好落在「快路径 + 简单纹理采样」
这个被特化的交集里 —— **A 路径（上屏合成）在 software 后端的像素 A/B 仍可信；
B 路径（任意片元逻辑，含 SDF）不可信**。

推论（对口径纪律的强化）：

- 「无硬件兜底口径」在 software 后端上连**正确性**都不可信，不只是性能 ——
  B 路径任何视觉一致性验证都必须 hardware runner。
- G-4 双闸门的"无硬件 runner 跳过逻辑"由此获得更硬的依据：不是性能豁免，是**执行语义豁免**。

本探针在 software 适配器上自动 skip（skip 理由写进代码注释），硬件复测待
gpu-hardware runner / 真机。

## 5. 探针结论（对 G-2 四问的回答）

| 探针问题 | 回答 | 置信度 |
|---|---|---|
| 文本字形图集内存与失效策略 | 全字号 44 MiB / 11 页，惰性建页可行；**LRU 必须换页管理** | 离线实测，高 |
| 批处理可行性 | fillrect 个位数/帧 + 颜色 ≤9 → instanced 单 draw 可覆盖；**第一阶段必须是 SDF 圆角** | 离线实测，高 |
| 视觉一致性 | present 合成已像素级一致（既有断言）；**绘制侧必须硬件验证**，software 后端不可作参照 | 实测证明其不可测性，高 |
| 体积与依赖 | +6.16MB，矩阵不变 | 沿用 11-08 核销，高 |

**建议：分阶段做（维持 G-0 拍板），但执行条件收紧为**：

1. 阶段 1（矩形纯色）= **SDF 圆角矩形** instanced 批处理，WGSL 已备（probeWGSL），
   开工前置条件 = gpu-hardware runner 或真机可用（像素 A/B 探针就位）；
2. 图集页管理方案（替换 glyphLRU）可与阶段 1 并行离线实施，不依赖硬件；
3. 每阶段沿用同一套像素 A/B 口径（本探针代码即模板），差异逐条说明，不许静默降阈值；
4. A 路径（上屏合成默认化 + swapchain 补齐）优先级仍高于本项（G-0 口径不变）。
