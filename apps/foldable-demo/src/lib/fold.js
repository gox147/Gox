// 纯逻辑：形态判定与文案格式化。不 import "gox"，可以被纯逻辑测试直接覆盖。
//
// 这一层刻意不碰任何 API —— 把"读数 → 字符串 / 布尔"单独拿出来，好让布局组件
// 只管排版，也让这里能被单测穷举。

// postureText 姿态 → 中文标签（姿态字符串来自 gx/screen 的 posture()）。
export function postureText(p) {
  if (p === "half-open") return "半折";
  if (p === "folded") return "完全折起";
  if (p === "flat") return "平展";
  return "未知";
}

// isTabletop 是**本次拍板的 Tabletop 判据**（2026-11-04，看板 rc2XuQ）：
// 半折 + 横折痕 = 桌面态。两条都要满足 —— 只看 posture 会把所有竖折半开
// 也判成 Tabletop，只看 orientation 会把平展横放的平板也判进来。
//
// 为什么不进 posture 枚举、也不用 layoutMode().suggested：
//   - posture 枚举对应的是"设备物理姿态"，Tabletop 是"姿态 + 折痕方向"的
//     组合，塞进枚举会让每个消费方都要再判一次方向，等于没抽象；
//   - suggested 是"该几栏"的建议（半折一律 dual），不携带方向语义 ——
//     拿 suggested === "tablet" 当 Tabletop 判据是**错的**（半折横屏时
//     suggested 仍是 "dual"）。
export function isTabletop(p, hg) {
  return p === "half-open" && !!hg && hg.orientation === "horizontal";
}

// panes 由宽度算"几栏"。**只信宽度，不信姿态** ——
// 上下折（Pocket 系列）展开后宽度可能仍 < 600dp，用姿态推必然翻车。
export function panes(width, minWidth) {
  const mw = minWidth > 0 ? minWidth : 600;
  return width >= mw ? 2 : 1;
}

// widthClassText 尺寸类 → 中文标签 + 阈值提示。
export function widthClassText(c) {
  if (c === "compact") return "compact（< 600dp）";
  if (c === "medium") return "medium（600–840dp）";
  if (c === "expanded") return "expanded（> 840dp）";
  return c || "—";
}

// hingeText 折痕 → 一行摘要。无折痕时明确说"无"，不要静默成 0。
export function hingeText(h) {
  if (!h) return "无折痕";
  return h.orientation + " · (" + h.x + "," + h.y + ") " + h.width + "×" + h.height;
}

// regionsText 保留区 → 一行摘要。division 是折痕那条带，occlusion 是挖孔一类遮挡。
export function regionsText(r) {
  if (!r) return "division 0 段 / occlusion 0 处";
  const d = (r.division || []).length;
  const o = (r.occlusion || []).length;
  return "division " + d + " 段 / occlusion " + o + " 处";
}

// logText 日志数组 → 单行（最多留最近 2 条，避免撑破状态条）。
export function logText(lines) {
  if (!lines || lines.length === 0) return "（还没有事件）";
  return lines.slice(-2).join("   ·   ");
}
