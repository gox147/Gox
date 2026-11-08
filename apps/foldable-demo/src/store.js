// 应用状态：形态上报 + 读数快照 + 事件日志。
//
// 桌面/无头宿主没有折叠姿态查询 API（Windows 没有 WinRT、X11 没有对应扩展），
// 框架的立场是**提供通道不猜姿态** —— 没人上报就恒为 flat。所以这个演示自己
// 扮演"宿主"：按钮点一下 = 往内核上报一次姿态，脚本侧读数随之变化。
// 真机上的上报者是平台壳工程（Android 的 DisplayManager / iOS 的
// UITraitCollection），脚本写法完全一样。
import { createSignal } from "gox";
import {
  posture,
  hinge,
  regions,
  splitRatio,
  hingeOrientation,
  reportPosture,
  resetDisplays,
  onWindowDisplayChange,
} from "gx/screen";
import { reservedRegions, hasFold, layoutMode, widthClass, onViewportChange } from "gx/viewport";
import { widthClassText, postureText } from "./lib/fold.js";

// ===== 演示窗口的"设备尺寸" =====
//
// 上报的显示器尺寸与窗口客户区保持一致 —— 折痕坐标是**显示器坐标**，两条链
// 用同一个原点，S2 的避让带才对得上（真机上全屏时二者同样重合）。
export const CANON_W = 900;
export const CANON_H = 760;

// ===== 形态：四种姿态 + 挖孔 + 非折叠基线 =====
//
// 每档 = 一次 reportPosture：尺寸、是否折叠设备、姿态、折痕、保留区。
// 竖折折痕是竖条，横折（Tabletop）折痕是横条，挖孔屏是屏下摄像头那块 occlusion。
export const FORMS = [
  {
    key: "flat",
    label: "平展",
    sub: "竖折展开",
    opts: {
      foldable: true,
      posture: "flat",
      hinge: { x: 440, y: 0, w: 0, h: CANON_H, orientation: "vertical" },
      regions: [
        { kind: "division", x: 440, y: 0, w: 0, h: CANON_H, active: false },
      ],
    },
  },
  {
    key: "half",
    label: "半折 · 竖",
    sub: "左右两段",
    opts: {
      foldable: true,
      posture: "half-open",
      hinge: { x: 440, y: 0, w: 40, h: CANON_H, orientation: "vertical" },
      regions: [
        { kind: "division", x: 440, y: 0, w: 40, h: CANON_H, active: true },
      ],
    },
  },
  {
    key: "tabletop",
    label: "半折 · 横",
    sub: "Tabletop",
    opts: {
      foldable: true,
      posture: "half-open",
      hinge: { x: 0, y: 380, w: CANON_W, h: 40, orientation: "horizontal" },
      regions: [
        { kind: "division", x: 0, y: 380, w: CANON_W, h: 40, active: true },
      ],
    },
  },
  {
    key: "occlusion",
    label: "挖孔屏",
    sub: "屏下摄像头",
    opts: {
      foldable: true,
      posture: "flat",
      hinge: { x: 440, y: 0, w: 0, h: CANON_H, orientation: "vertical" },
      regions: [
        { kind: "division", x: 440, y: 0, w: 0, h: CANON_H, active: false },
        { kind: "occlusion", x: 420, y: 0, w: 64, h: 64, active: true },
      ],
    },
  },
  {
    key: "plain",
    label: "非折叠",
    sub: "普通平板",
    opts: { foldable: false, posture: "flat" },
  },
];

// ===== signal =====
const formP = createSignal("half");
const logP = createSignal([]);
const snapP = createSignal("");

export const form = formP[0];
export const log = logP[0];
export const snap = snapP[0];

function pushLog(line) {
  logP[1](log().concat([line]));
}

let win = null;

// bindWindow 记录窗口句柄并上报首帧形态。
//
// **必须推迟到事件循环里调用**（main.js 用 setTimeout(..., 0)）：reportPosture
// 会同步派发订阅回调（object.CallFunction 重入 VM），在模块顶层求值期间重入
// 会让本次执行余下的语句静默失效 —— 表现为"界面停在无折痕的零态，点什么都
// 读到平展"。
export function bindWindow(w) {
  win = w;
  applyForm("half");
}

export function windowHandle() {
  return win;
}

// applyForm 切到某个形态：回到标准尺寸 → 清掉上次覆盖 → 上报 → 记日志 → 刷快照。
export function applyForm(key) {
  const f = FORMS.find((x) => x.key === key);
  if (!f) return;
  // 每种形态都按"整机 900×760"演示：S1 里手动缩过窗口后，切形态要回到
  // 基准几何，否则折痕坐标会落到窗口外（避让带自然不生效，看起来像坏了）。
  if (win) win.resize(CANON_W, CANON_H);
  resetDisplays();
  const opts = Object.assign({ width: CANON_W, height: CANON_H }, f.opts);
  reportPosture(opts);
  formP[1](key);
  pushLog("形态 → " + f.label);
  refreshSnap();
}

// refreshSnap 把全部读数取一次拼成多行文本。
//
// 注意这里全是**纯读数**（posture() / hinge() / layoutMode() …）—— 它们不会
// 建立订阅，读一次就是"此刻的值"。要跟着环境自动更新，用的是下面
// onViewportChange 注册的回调 + 把读数写进 signal。
export function refreshSnap() {
  const p = posture(win);
  const h = hinge(win);
  const r = reservedRegions(win);
  const m = layoutMode(win);
  const lines = [
    "posture()          = " + p + "（" + postureText(p) + "）",
    "hingeOrientation() = " + hingeOrientation(win),
    "splitRatio()       = " + splitRatio(win).toFixed(3) + "（钳 [0.2, 0.8]）",
    "hasFold()          = " + hasFold(win) + "（结构性，不随折叠 flip-flop）",
    "widthClass()       = " + widthClassText(widthClass(win)),
    "layoutMode()       = " + JSON.stringify(m),
    "reservedRegions()  = division " + r.division.length + " 段 / occlusion " +
      r.occlusion.length + " 处",
    "regions()          = " + regions(win).length + " 段",
  ];
  snapP[1](lines.join("\n"));
}

// 环境一变（上报姿态 / 转屏 / 窗口挪屏）就刷快照 —— "响应式"的接线方式。
onViewportChange(() => refreshSnap());

// 跨屏事件：窗口换屏时记一行（真机双屏/扩展屏宿主会真的触发）。
onWindowDisplayChange((e) => {
  pushLog("换屏 " + e.fromDisplay + " → " + e.toDisplay);
  refreshSnap();
});

export { refreshSnap as refresh };
