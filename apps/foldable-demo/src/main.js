// 入口：一个窗口，四个折叠屏场景（单场景舞台，逐个切换）。
//
// 窗口开到 900×760（与上报的"整机尺寸"一致），但**列数由窗口真实宽度决定** ——
// 点 S1 的「窗口 520」，列表自己从双栏变单栏，这份代码一个字都不用改。
import { h, render } from "gox";
import { App } from "./app.js";
import { bindWindow } from "./store.js";

const WIN_W = 900;
const WIN_H = 760;

const win = render(
  <window title="foldable-demo · 折叠屏四场景" width={WIN_W} height={WIN_H}>
    <App />
  </window>
);

// 首帧之后再上报形态 —— 为什么不能直接同步调：
// reportPosture 会同步派发订阅回调（object.CallFunction 重入 VM）。在模块
// 顶层求值期间重入，本次执行余下的语句会静默失效：界面停在"无折痕 + 0px"
// 的零态，按钮点了也只读到平展，而且**没有任何报错**。推迟到事件循环
// （定时器 0ms）里调用，重入发生在正常回调栈上，读数与界面都对。
setTimeout(() => bindWindow(win), 0);
