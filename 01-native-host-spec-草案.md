# 01 · 原生宿主接入规格（草案）

> **状态**：草案 / 工作稿，随实现演进。最近一次修订：2026-11-04（看板 `rc2XuQ` 拍板回填 §4.1）。
> **配套**：[`docs/foldable-adaptation.md`](docs/foldable-adaptation.md)（应用侧适配指南）、
> [`apps/foldable-demo`](apps/foldable-demo)（四场景可跑 demo）。
> **范围**：平台壳工程（Android / iOS / 鸿蒙 / 桌面模拟器）如何把"折叠屏几何"报给 Gox 内核，
> 以及脚本侧能读到什么。**不含**应用怎么做布局（那是适配指南的事）。

---

## 1. 术语

| 词 | 含义 | 备注 |
|---|---|---|
| posture | 设备物理姿态：`flat` / `half-open` / `folded` / `unknown` | 只描述"折没折"，不含方向 |
| hinge | 折痕：位置 + 尺寸 + `orientation`（`vertical` / `horizontal`） | 设备级几何，平展时也在 |
| division | 折痕那条**不可用带**（保留区 kind） | 平展时宽度 0、`active=false` |
| occlusion | 遮挡区（屏下摄像头一类） | 与折不折无关，永远避让 |
| Tabletop | 半折 + 横折痕 = 放桌上"上屏下台"的形态 | **不是** posture 枚举值，见 §4.1 |

## 2. 上报通道

两条，语义完全一致，只是入口不同：

```text
平台壳（Android DisplayManager / iOS UITraitCollection / 鸿蒙）
        │
        ├── gfx.ReportPostureFromFold(gfx.Display{...})     // Go 强类型，平台侧用这条
        └── reportPosture({ posture, hinge, regions, ... }) // JS，模拟器/开发者工具/demo 用这条
                    │
                    ▼
         upsertDisplayLocked → commitDisplaysLocked → notifyEnvChanged
                    │
                    ▼
         gx/screen（读数 + 订阅） · gx/viewport（布局建议 + 断点） · gfx/layout（avoidReserved）
```

**线程**：`ReportPostureFromFold` 与 `reportPosture` 都会同步派发脚本订阅回调，
只能在 GUI 线程调用；平台回调必须经 `gfx.Post` 转投。

**合并语义**：只覆盖本次给到的字段，没给的沿用旧值。折痕**不随姿态清除** —— 平展时
仍保留 `hinge`，否则"折回去再折回来"分栏比例会跳回 0.5。

## 3. 字段与单位

| 字段 | 单位 | 说明 |
|---|---|---|
| `width` / `height` | 设备像素 | 首次上报必须带，否则"窗口所在显示器"解析不到 |
| `hinge.x/y/w/h` | 设备像素，显示器坐标 | 平展时 `w=0`，不要省略 |
| `regions[].x/y/width/height` | 设备像素，显示器坐标 | 布局属性是 dp，脚本侧要先除 `deviceInfo().pixelRatio` |
| `regions[].active` | 布尔 | 不传时按"零宽/零高即 inactive"推导 |

## 4. 形态派生

### 4.1 Tabletop 的归属（2026-11-04 拍板）

**结论：不进 posture 枚举，不进 `layoutMode().suggested`，在 demo/应用层用组合判据。**

```js
// 唯一判据：半折 + 横折痕，两条同时成立
const isTabletop =
  posture() === "half-open" &&
  hinge()?.orientation === "horizontal";
```

为什么不新增枚举值、为什么不复用建议值：

1. **`posture` 是"设备物理姿态"**，Tabletop 是"姿态 × 折痕方向"的组合。把组合塞进
   枚举，等于让每个消费方再判一次方向 —— 抽象没换来任何东西，还多了一个要维护的状态。
2. **`layoutMode().suggested` 是"该几栏"的建议，不携带方向语义**。半折横屏时
   `suggested` 仍然是 `"dual"`（两块可用区，建议分两块），拿 `suggested === "tablet"`
   当 Tabletop 判据是**错的**：`tablet` 的含义是"宽档但没折"，与 Tabletop 正交。
3. **框架立场是"只给判定，不改布局"**。Tabletop 该长什么样（上预览下操作台 / 上地图
   下表单）取决于页面信息架构，框架替应用决定只会猜错。

因此：

| 位置 | 责任 |
|---|---|
| 平台壳 | 只报 `posture` / `hinge` / `regions`，**不判断 Tabletop** |
| 内核 | 提供读数与订阅，`layoutMode().suggested` 保持"single / dual / tablet"三值不动 |
| 应用 | 用 §4.1 的组合判据决定分屏方式（参考实现见 §4.2） |

### 4.2 参考实现

```js
import { posture, hinge, usePosture } from "gx/screen";
import { isTabletop } from "./lib/fold.js";   // 纯函数，可单测

const getP = usePosture();                    // 订阅型读数：无条件先取一次
const tt   = () => isTabletop(getP(), hinge());  // 每次重算都走同一条判据

return () => tt()
  ? <column>…上预览 + 横折痕 + 下操作台…</column>
  : <column>…普通单栏…</column>;
```

`isTabletop(posture, hinge)` 落在 `apps/foldable-demo/src/lib/fold.js`，是**纯函数**：
不 import `gox`、不读全局，两个入参就能穷举真值表（半折×竖 / 半折×横 / 平展×横 / 无折痕）。

### 4.3 各端上报注意

| 平台 | 上报源 | 坑 |
|---|---|---|
| Android | `DisplayManager` + `FoldingFeature`（Jetpack WindowManager） | `FoldingFeature.orientation` 映射到 `vertical`/`horizontal` 时注意**摄像头视角 vs 屏幕坐标**；`state=FLAT` 时报 `w=0` 的 division |
| iOS | `UIWindowScene` 无铰链 API → 由壳工程按设备表回填 | 不要把 `traitCollection` 的 size class 当姿态 |
| 鸿蒙 | 折叠屏 `displayFoldStatus` | `media.preview` 在鸿蒙 unsupported，演示别依赖它 |
| 桌面 | 模拟器/开发者工具调 `reportPosture` | 首次上报必须带 `width/height`，否则读数是空的 |

### 4.4 变更影响面

新增/修改 posture 相关语义时，需要同步：

- `gfx/screen.go`（读数与上报）· `gfx/viewport.go`（`layoutMode` / `reservedRegions`）
- `gfx/layout.go`（`avoidReserved` 的可用区计算）
- `apps/foldable-demo`（四场景）· `testdata/foldable_demo.js`（八形态矩阵）
- 回归：`gfx/foldable_demo_test.go`、`gfx/foldable_showcase_test.go`、`gfx/reserved_test.go`

## 5. 验收口径

1. 同一段脚本在 4 种形态（平展 / 半折竖 / 半折横 / 挖孔）下读数正确、布局不崩。
2. 姿态变化后**布局当场重排**（折痕避让不能等下一次 resize 才生效 —— 这是 2026-11-07
   修掉的一个真实缺陷，见 `gfx/screen.go` 的 `notifyEnvChanged`）。
3. 30 秒演示物料可复现：`GOX_FOLDABLE_SHOTS=docs/foldable-demo go test ./gfx -run TestFoldableShowcase`。
