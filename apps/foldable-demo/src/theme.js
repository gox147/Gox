// 设计令牌 —— 每个应用自带一份，不共享。
export const colors = {
  bg: "#eef2f7",
  panel: "#ffffff",
  panelAlt: "#f7f9fc",
  border: "#d8e0ea",
  text: "#1b2430",
  muted: "#697586",
  faint: "#9aa6b6",
  accent: "#0b7a75",
  accentSoft: "#dff1ef",
  accentFg: "#ffffff",
  // 折痕避让的可视化：折痕带用暖色，与内容区明确区分
  hinge: "#f2d3ae",
  hingeBorder: "#c99a6b",
  // 两块"物理屏"的底色
  left: "#e8f0fb",
  right: "#eaf6ee",
  // 告警（未避让）
  warn: "#b45309",
  warnSoft: "#fdf0e2",
};

export const space = { xs: 4, sm: 6, md: 10, lg: 14, xl: 18 };

export const font = { xs: 10, sm: 11, md: 13, lg: 15, xl: 19, xxl: 24 };

export const radius = { sm: 4, md: 8, lg: 12 };
