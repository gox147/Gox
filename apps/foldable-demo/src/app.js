// 根组件：折叠屏四场景（单场景舞台，适合录屏/演示）。
//
// 每个场景回答一个具体问题：
//
//   S1 单双栏自适应   列数只信宽度（useWindowInfo / widthClass），不信姿态
//   S2 折痕/挖孔避让  division（折痕带）与 occlusion（屏下摄像头）两种都演
//   S3 Tabletop       isTabletop = 半折 + 横折痕 → 上预览 / 下操作台
//   S4 跨屏拖拽       窗口换屏时 screenOf() / bounds().displayId 的变化
//
// 纪律（四场景共用）：
//   ① 布局只信宽度；② 订阅型读数先取再分支，别塞进三元；③ 折痕/保留区坐标
//   是显示器坐标；④ 没人上报就恒为平展 —— 真机由平台壳工程上报。
import { createSignal } from "gox";
import {
  useBreakpoint,
  useReservedRegions,
  widthClass,
  layoutMode,
  hasFold,
  reservedRegions,
} from "gx/viewport";
import {
  posture,
  hinge,
  usePosture,
  screens,
  screenOf,
  useWindowInfo,
  onWindowDisplayChange,
} from "gx/screen";
import { colors, space, font, radius } from "./theme.js";
import {
  FORMS,
  form,
  applyForm,
  log,
  snap,
  refresh,
  windowHandle,
  CANON_W,
  CANON_H,
} from "./store.js";
import {
  postureText,
  isTabletop,
  panes,
  widthClassText,
  hingeText,
  regionsText,
  logText,
} from "./lib/fold.js";

// ===== 通用小件 =====

function Card(p) {
  return (
    <column gap={space.sm} padding={space.md} background={colors.panel}
            border={colors.border} radius={radius.md}
            height={p.height ? p.height : undefined} flexGrow={p.grow ? 1 : 0}>
      {p.title ? <text font={font.md} color={colors.text}>{p.title}</text> : null}
      {p.children}
    </column>
  );
}

function KV(p) {
  return (
    <row gap={space.xs} alignItems="center">
      <text font={font.xs} color={colors.muted} width={p.kw ? p.kw : 120}>{p.k}</text>
      <text font={font.xs} color={colors.text}>{p.v}</text>
    </row>
  );
}

function SceneHead(p) {
  return (
    <row gap={space.sm} alignItems="center">
      <tag color={colors.accent}>{p.n}</tag>
      <text font={font.lg} color={colors.text}>{p.title}</text>
      <text font={font.xs} color={colors.muted}>{p.hint}</text>
    </row>
  );
}

// ===== S1：单双栏自适应（只信宽度）=====

const MAILS = [
  { id: 1, from: "设计组", subject: "折叠屏稿 v3 已上传" },
  { id: 2, from: "CI", subject: "夜间回归：全绿" },
  { id: 3, from: "某产品", subject: "Tabletop 场景确认" },
  { id: 4, from: "平台壳", subject: "姿态上报联调日志" },
  { id: 5, from: "QA", subject: "挖孔屏机型借用" },
  { id: 6, from: "文档", subject: "适配指南增补" },
  { id: 7, from: "内核", subject: "avoidReserved 边界" },
  { id: 8, from: "运维", subject: "构建机磁盘告警" },
];

function MailRow(p) {
  return (
    <row gap={space.sm} alignItems="center" padding={space.xs}>
      <row width={6} height={6} radius={3} background={p.unread ? colors.accent : colors.border} />
      <column gap={1} flexGrow={1}>
        <text font={font.sm} color={colors.text}>{p.from}</text>
        <text font={font.xs} color={colors.muted}>{p.subject}</text>
      </column>
      <text font={font.xs} color={colors.faint}>{p.time}</text>
    </row>
  );
}

// S1 的"窗口宽度"是演示信号：录制/离屏环境下没法真的拖窗口边缘，
// 但**列数判据用的仍然是同一个 panes(width)** —— 真机上 width 来自
// useWindowInfo()（窗口 resize 事件），逻辑一字不改。
const demoWP = createSignal(900);
export const demoW = demoWP[0];

function adaptiveScene(width) {
  const wide = width >= 600;

  const col = (from, to) => (
    <column gap={2} flexGrow={1}>
      <view each={MAILS.slice(from, to)} key="id">
        {(m) => (
          <MailRow from={m.from} subject={m.subject} unread={m.id <= 2}
                   time={"09:" + (m.id < 10 ? "0" + m.id : m.id)} />
        )}
      </view>
    </column>
  );

  const list = (
    <column gap={space.sm} padding={space.md} background={colors.panel}
            border={colors.border} radius={radius.md} flexGrow={1}>
      <row alignItems="center">
        <text font={font.md} color={colors.text}>收件箱</text>
        <spacer flexGrow={1} />
        <text font={font.xs} color={colors.muted}>{panes(width) === 1 ? "单栏" : "双栏"}</text>
      </row>
      {wide
        ? <row gap={space.md} alignItems="top">{col(0, 4)}{col(4, 8)}</row>
        : <column gap={2}>{col(0, 8)}</column>}
      <text font={font.xs} color={colors.muted}>
        {"当前 " + panes(width) + " 栏 · 窗口 " + width + " px"}
      </text>
    </column>
  );

  const info = (
    <row gap={space.md} padding={space.md} background={colors.panel} border={colors.border}
         radius={radius.md} alignItems="center">
      <text font={font.md} color={colors.text}>窗口尺寸</text>
      <KV k="width" kw={44} v={width + " px"} />
      <KV k="widthClass" kw={66} v={width >= 840 ? "expanded" : width >= 600 ? "medium" : "compact"} />
      <KV k="panes" kw={42} v={panes(width) + " 栏"} />
      <spacer flexGrow={1} />
      <button padding={space.sm} onClick={() => demoWP[1](520)} background={colors.panelAlt}>
        <text font={font.xs} color={colors.text}>窗口 520</text>
      </button>
      <button padding={space.sm} onClick={() => demoWP[1](720)} background={colors.panelAlt}>
        <text font={font.xs} color={colors.text}>窗口 720</text>
      </button>
      <button padding={space.sm} onClick={() => demoWP[1](900)} background={colors.accent}>
        <text font={font.xs} color={colors.accentFg}>窗口 900</text>
      </button>
    </row>
  );

  return (
    <column gap={space.md} height={470}>
      <SceneHead n="S1" title="单双栏自适应"
                 hint="列数只由宽度决定 —— 用 posture 推布局必然翻车" />
      <row gap={space.md} alignItems="stretch" height={370}>
        <column width={width}>{list}</column>
      </row>
      {info}
    </column>
  );
}

// ===== S2：折痕 / 挖孔避让 =====

const CHIPS = [
  { id: "c1", label: "收藏" },
  { id: "c2", label: "归档" },
  { id: "c3", label: "转发" },
  { id: "c4", label: "删除" },
];

function ChipRow(p) {
  return (
    <row gap={space.xs} alignItems="center">
      <button padding={space.sm} background={p.tone ? p.tone : colors.panelAlt}>
        <text font={font.xs} color={colors.text}>{p.label}</text>
      </button>
      <text font={font.xs} color={colors.muted}>{p.hint}</text>
    </row>
  );
}

function ScenarioReserved() {
  // 两段式：先无条件取一次订阅型读数（getter），再进分支。
  const getP = usePosture();
  const getR = useReservedRegions();
  const hg = () => hinge(windowHandle());
  const rr = () => {
    getP(); // 形态一变，折痕/保留区都要跟着重算
    return getR ? getR() : reservedRegions(windowHandle());
  };

  // 折痕/挖孔在**窗口坐标**里的位置：上报的显示器原点与窗口对齐，故直接读。
  const bandX = () => {
    const h = hg();
    if (h && h.orientation === "vertical" && h.width > 0) return h.x;
    const occ = (rr().occlusion || []).find((r) => r.active);
    return occ ? occ.x : -1;
  };
  const bandW = () => {
    const h = hg();
    if (h && h.orientation === "vertical" && h.width > 0) return h.width;
    const occ = (rr().occlusion || []).find((r) => r.active);
    return occ ? occ.width : 6;
  };

  const Rows = (p) => (
    <column gap={space.xs} width={p.width ? p.width : 300} avoidReserved={p.avoid ? "all" : undefined}>
      <view each={CHIPS} key="id">
        {(c) => <ChipRow label={c.label} hint={p.hint} tone={p.tone} />}
      </view>
    </column>
  );

  return (
    <column gap={space.md} flexGrow={1} height={"100%"}>
      <SceneHead n="S2" title="折痕 / 挖孔避让"
                 hint="division 与 occlusion 两种 kind 都演，避让是 opt-in" />
      <Card height={350} title={() => "保留区：" + regionsText(rr()) + "  ·  折痕 " + hingeText(hg())}>
        <text font={font.xs} color={colors.muted}>折痕位置（按窗口坐标，整条 = 窗口宽）</text>
        <row height={14} background={colors.panelAlt} radius={7}>
          {() => {
            const x = bandX();
            if (x < 0) {
              return <text font={font.xs} color={colors.faint}>（本形态没有竖折痕 / 挖孔）</text>;
            }
            // 标尺左端 = 舞台 padding(10) + 卡片 padding(10) + 卡片边框(1)
            const origin = 21;
            const left = x - origin > 0 ? x - origin : 0;
            return (
              <row gap={0} height={14}>
                <spacer width={left} />
                <row width={bandW()} height={14} background={colors.hinge} radius={3} />
              </row>
            );
          }}
        </row>

        <row gap={space.lg} alignItems="top">
          <column gap={space.xs} flexGrow={1}>
            <text font={font.xs} color={colors.warn}>未避让：内容压在保留带上</text>
            <row padding={space.sm} background={colors.warnSoft} radius={radius.sm} height={158}>
              <Rows hint="落在折痕/挖孔下面" />
            </row>
          </column>

          <column gap={space.xs} flexGrow={1}>
            <text font={font.xs} color={colors.accent}>已避让：容器写 avoidReserved="all"</text>
            <row padding={space.sm} background={colors.accentSoft} radius={radius.sm} height={158}>
              <Rows avoid hint="整组让到保留带右边" tone={colors.panel} />
            </row>
          </column>
        </row>

        <text font={font.xs} color={colors.muted} wrap width={720}>
          折痕带是"分隔"不是"遮挡"，所以避让做成显式 opt-in：全屏视频/背景图恰恰应该铺到折痕下面。
        </text>
      </Card>

      <Card title="写法">
        <row gap={space.md} alignItems="center">
          <column gap={2} padding={space.sm} background={colors.panelAlt} radius={radius.sm}
                  flexGrow={1}>
            <text font={font.xs} color={colors.text}>
              {'<column avoidReserved="all">   // division + occlusion 都避开'}
            </text>
            <text font={font.xs} color={colors.muted}>
              {'  <row>…工具条…</row>          // 整组让到保留带右侧'}
            </text>
            <text font={font.xs} color={colors.muted}>
              {'</column>'}
            </text>
          </column>
          <text font={font.xs} color={colors.muted} wrap width={280}>
            取值只有 division / occlusion / all 三个词，拼错**静默不生效** ——
            误用的代价是"莫名多出一段空白"，收窄词表比猜方向更好排查。
          </text>
        </row>
      </Card>
    </column>
  );
}

// ===== S3：Tabletop =====

const FRAMES = ["01 / 03", "02 / 03", "03 / 03"];

function ScenarioTabletop() {
  const getP = usePosture();
  const hg = () => hinge(windowHandle());
  const tabletop = () => isTabletop(getP(), hg());

  const [idx, setIdx] = createSignal(0);
  const [playing, setPlaying] = createSignal(true);

  const step = (d) => {
    setIdx((idx() + d + FRAMES.length) % FRAMES.length);
  };

  const Controls = () => (
    <row gap={space.sm} alignItems="center" justifyContent="center" flexGrow={1}>
      <button padding={space.sm} onClick={() => step(-1)} background={colors.panelAlt}>
        <text font={font.sm} color={colors.text}>◀◀</text>
      </button>
      <button padding={space.sm} onClick={() => setPlaying(!playing())} background={colors.accent}>
        <text font={font.md} color={colors.accentFg}>{() => (playing() ? "❚❚" : "▶")}</text>
      </button>
      <button padding={space.sm} onClick={() => step(1)} background={colors.panelAlt}>
        <text font={font.sm} color={colors.text}>▶▶</text>
      </button>
      <text font={font.xs} color={colors.muted}>{() => "画面 " + FRAMES[idx()]}</text>
    </row>
  );

  return (
    <column gap={space.md} flexGrow={1} height={"100%"}>
      <SceneHead n="S3" title="Tabletop（上预览 / 下操作台）"
                 hint="isTabletop = 半折 且 横折痕 —— 不新增枚举，demo 层组合" />
      {() => (tabletop() ? (
        <column flexGrow={1} gap={0}>
          <row height={340} background={colors.left} radius={radius.md} alignItems="center"
               justifyContent="center">
            <column gap={space.xs} alignItems="center">
              <text font={font.xxl} color={colors.accent}>▶</text>
              <text font={font.md} color={colors.text}>{"画面 " + FRAMES[idx()]}</text>
              <text font={font.xs} color={colors.muted}>上段：内容 / 画面（半折的上面那块）</text>
            </column>
          </row>
          <row height={40} background={colors.hinge} border={colors.hingeBorder} alignItems="center"
               justifyContent="center">
            <text font={font.xs} color={colors.hingeBorder}>折痕（横）· 上下两段之间不可用</text>
          </row>
          <row height={96} background={colors.right} radius={radius.md} alignItems="center"
               padding={space.md}>
            <Controls />
          </row>
        </column>
      ) : (
        <Card grow title="当前不是 Tabletop">
          <text font={font.sm} color={colors.text}>
            {"posture = " + getP() + " · 折痕 = " + (hg() ? hg().orientation : "无")}
          </text>
          <text font={font.xs} color={colors.muted} wrap width={680}>
            Tabletop 需要「半折 + 横折痕」两个条件同时成立。当前形态下同一段代码走
            单栏分支 —— 这正是验收要的：同一份代码，形态变了布局自己变。
          </text>
          <button padding={space.sm} onClick={() => applyForm("tabletop")} width={170}
                  background={colors.accent}>
            <text font={font.sm} color={colors.accentFg}>切到「半折 · 横」</text>
          </button>
        </Card>
      ))}
      <text font={font.xs} color={colors.muted} wrap width={700}>
        下半屏用自绘控制条，不用 media.preview（该 API 在鸿蒙显式 unsupported，
        用它鸿蒙就演不了）。判据只读 posture + 折痕方向，不碰 layoutMode().suggested
        —— 半折横屏时 suggested 仍然是 "dual"。
      </text>
    </column>
  );
}

// ===== S4：跨屏拖拽 =====

function ScenarioCrossDisplay() {
  const [evt, setEvt] = createSignal("（把窗口挪到另一块屏会出现记录）");
  onWindowDisplayChange((e) => {
    setEvt("win " + e.windowId + "：" + e.fromDisplay + " → " + e.toDisplay);
  });

  const list = () => screens();
  const cur = () => screenOf();
  const geo = () => {
    const win = windowHandle();
    if (!win) return { x: 0, y: 0, width: 0, height: 0, displayId: "-", scale: 1 };
    return win.bounds();
  };

  const moveBy = (dx) => {
    const g = geo();
    const win = windowHandle();
    if (win) win.moveTo(g.x + dx, g.y);
  };

  return (
    <column gap={space.md} flexGrow={1} height={"100%"}>
      <SceneHead n="S4" title="跨屏拖拽（窗口换物理屏）"
                 hint="P1/P2：复用 multiscreen 的跨屏几何，不做半屏间拖拽" />
      <row gap={space.md} height={300} alignItems="stretch">
        <Card grow title="显示器">
          {() => {
            const ds = list();
            if (!ds || ds.length === 0) {
              return <text font={font.xs} color={colors.muted}>（宿主没报显示器）</text>;
            }
            return (
              <column gap={space.xs}>
                <view each={ds} key="id">
                  {(d) => (
                    <row gap={space.sm} padding={space.sm} radius={radius.sm}
                         background={d.id === (cur() && cur().id) ? colors.accentSoft : colors.panelAlt}
                         border={d.id === (cur() && cur().id) ? colors.accent : colors.border}>
                      <row width={8} height={8} radius={4}
                           background={d.id === (cur() && cur().id) ? colors.accent : colors.faint} />
                      <text font={font.sm} color={colors.text} width={120}>{d.id}</text>
                      <text font={font.xs} color={colors.muted}>
                        {d.width + "×" + d.height + "  @" + d.x + "," + d.y + "  ×" + d.scale}
                      </text>
                      <spacer flexGrow={1} />
                      <text font={font.xs} color={d.primary ? colors.accent : colors.faint}>
                        {d.primary ? "主屏" : "扩展"}
                      </text>
                    </row>
                  )}
                </view>
                <text font={font.xs} color={colors.muted}>布局示意（1/8 缩放，绿框 = 窗口所在屏）</text>
                <row gap={space.sm} height={120} background={colors.bg} padding={space.sm}
                     radius={radius.sm} alignItems="center">
                  <view each={ds} key="id">
                    {(d) => (
                      <column width={d.width / 8} height={d.height / 8}
                              background={d.id === (cur() && cur().id) ? colors.accentSoft : colors.panel}
                              border={d.id === (cur() && cur().id) ? colors.accent : colors.border}
                              radius={radius.sm} alignItems="center" justifyContent="center">
                        <text font={font.xs} color={colors.muted}>{d.id}</text>
                      </column>
                    )}
                  </view>
                </row>
              </column>
            );
          }}
        </Card>

        <column gap={space.sm} width={288}>
          <Card title="窗口几何">
            <KV k="screenOf()" v={() => (cur() && cur().id ? cur().id : "—")} />
            <KV k="bounds.x / y" v={() => geo().x + " , " + geo().y} />
            <KV k="bounds.w / h" v={() => geo().width + " × " + geo().height} />
            <KV k="displayId" v={() => geo().displayId || "—"} />
            <row gap={space.xs}>
              <button padding={space.sm} onClick={() => moveBy(-160)} background={colors.panelAlt}>
                <text font={font.xs} color={colors.text}>左移 160</text>
              </button>
              <button padding={space.sm} onClick={() => moveBy(160)} background={colors.accent}>
                <text font={font.xs} color={colors.accentFg}>右移 160</text>
              </button>
            </row>
          </Card>
          <Card title="换屏事件">
            <text font={font.xs} color={colors.text} wrap width={256}>{() => evt()}</text>
          </Card>
        </column>
      </row>
      <text font={font.xs} color={colors.muted} wrap width={700}>
        双屏宿主上把窗口拖过去（或 moveTo 到另一块屏的工作区）就会触发
        onWindowDisplayChange；屏幕内左右半屏的拖拽是 v2，不在本次范围。
      </text>
    </column>
  );
}

// ===== 顶栏 / 形态条 / 页签 / 状态条 =====

function FormBar() {
  return (
    <row gap={space.xs} padding={space.sm} background={colors.panel} border={colors.border}
         alignItems="center">
      <text font={font.xs} color={colors.muted} width={64}>宿主上报</text>
      <view each={FORMS} key="key">
        {(f) => (
          <button padding={space.sm} onClick={() => applyForm(f.key)}
                  background={() => (form() === f.key ? colors.accent : colors.panelAlt)}
                  border={() => (form() === f.key ? colors.accent : colors.border)}>
            <text font={font.xs} color={() => (form() === f.key ? colors.accentFg : colors.text)}>
              {f.label}
            </text>
          </button>
        )}
      </view>
      <spacer flexGrow={1} />
      <text font={font.xs} color={colors.muted}>
        {() => "posture = " + postureText(posture(windowHandle()))}
      </text>
    </row>
  );
}

const SCENES = ["S1 单双栏", "S2 折痕避让", "S3 Tabletop", "S4 跨屏"];

function Tabs() {
  return (
    <row gap={0}>
      <view each={SCENES} key="id">
        {(name) => (
          <button padding={space.sm} borderless={true}
                  background={() => (scene() === SCENES.indexOf(name) ? colors.panel : colors.bg)}
                  border={() => (scene() === SCENES.indexOf(name) ? colors.accent : colors.border)}
                  onClick={() => sceneP[1](SCENES.indexOf(name))}>
            <text font={font.sm}
                  color={() => (scene() === SCENES.indexOf(name) ? colors.accent : colors.muted)}>
              {name}
            </text>
          </button>
        )}
      </view>
    </row>
  );
}

function StatusBar() {
  // 纯读数不会订阅 —— 状态条要跟着形态变，必须自己挂一个订阅型读数。
  const getP = usePosture();
  const tick = () => {
    getP();
  };
  return (
    <row gap={space.md} padding={space.sm} background={colors.panel} border={colors.border}
         alignItems="center">
      <text font={font.xs} color={colors.muted}>事件</text>
      <text font={font.xs} color={colors.text} flexGrow={1}>{() => logText(log())}</text>
      <text font={font.xs} color={colors.faint}>
        {() => {
          tick();
          return hasFold(windowHandle()) ? "带折痕" : "无折痕";
        }}
      </text>
      <text font={font.xs} color={colors.faint}>
        {() => {
          tick();
          return "suggested = " + layoutMode(windowHandle()).suggested;
        }}
      </text>
    </row>
  );
}

// ===== 页签状态（组件内 UI 状态，不进 store）=====

const sceneP = createSignal(0);
const scene = sceneP[0];

export function App() {
  // 根组件订阅窗口宽度：宽度一档变化，整棵场景树重建 —— 单双栏是**两棵不同
  // 的树**（不是同一棵树硬改），折痕/尺寸相关的排版也一起跟着重来。
  const getWin = useWindowInfo();
  return (
    <column background={colors.bg} flexGrow={1} height={CANON_H}>
      <row gap={space.sm} padding={space.md} background={colors.panel} border={colors.border}
           alignItems="center">
        <text font={font.xl} color={colors.text}>折叠屏适配 · 四场景</text>
        <text font={font.xs} color={colors.muted}>
          同一段代码 · 单双栏 / 折痕避让 / Tabletop / 跨屏
        </text>
        <spacer flexGrow={1} />
        <tag color={colors.accent}>{() => "整机 " + CANON_W + "×" + CANON_H}</tag>
      </row>
      <FormBar />
      <Tabs />
      <column flexGrow={1} padding={space.md} height={"100%"}>
        {() => {
          const i = scene();
          getWin().width; // 订阅：窗口变宽变窄都重建当前场景（见函数头注释）
          if (i === 0) return adaptiveScene(demoW());
          if (i === 1) return <ScenarioReserved />;
          if (i === 2) return <ScenarioTabletop />;
          return <ScenarioCrossDisplay />;
        }}
      </column>
      <StatusBar />
    </column>
  );
}
