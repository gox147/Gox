#!/usr/bin/env python3
"""性能对外数字单一真相源 —— 从 bench-results/*.json 生成 README / docs 里的数字块（看板 rrIdLN / T3-7）。

## 为什么需要它

`docs/performance.md` 被声明为「数字真相源」，但 README（含英文版）、promo 文章、
官网都各自**手抄**过数字。手抄的必然结局是漂移：本仓已经发生过一次
（README 停在 2026-11-14 CI 快照 247ms，而 docs §3.2 已经写到 11-17 夜批 201ms）。
数字是宣传材料，漂移一次就伤一次可信度（rHEQlb 的合规率徽章基线不可复现是同一类事故）。

所以：**数字只从 JSON 生成，文档里的数字块由本脚本维护**。
`--check` 模式把「文档里的块」与「从 JSON 重新生成的结果」逐字比对，
不一致即红 —— 这就是 r3FFt6「导出名单落成 golden 文件」思路在性能数字上的复用。

## 单一真相源（golden）

    docs/perf-numbers.json     ← 由本脚本 `--record` 从 bench-results 的原始 JSON 提炼，
                                 带出处（source 文件 + host + 日期 + commit）

**不要手改** `docs/perf-numbers.json`：它是生成的。要换数字，先跑基准拿到新 JSON，
再 `--record` 重新提炼，最后在文档里重跑生成块。

## 生成块（marker）

文档里用成对标记包住需要自动维护的数字，标记之间的内容由本脚本整体重写：

    <!-- perf-numbers:begin <id> -->
    ...由本脚本生成，手改会被 --check 判红...
    <!-- perf-numbers:end <id> -->

当前 id：
- `readme-quad` / `readme-quad-en`：README 四栈对照表 + 口径脚注
- `readme-conclusion` / `readme-conclusion-en`：README「结论」段
- `readme-threshold` / `readme-threshold-en`：README 前文「不适合 CPU 密集」的阈值数字

## 用法

    python3 scripts/gen-perf-numbers.py --record            # 从最新结果重建 golden
    python3 scripts/gen-perf-numbers.py --write             # 用 golden 重写文档里的块
    python3 scripts/gen-perf-numbers.py --check             # 校验文档块 == golden（CI/单测用）
    python3 scripts/gen-perf-numbers.py --print readme-quad # 打印某块内容

退出码：0 = 一致/成功；1 = 文档块与 golden 不一致（或标记缺失/重复）；2 = 数据或用法错误。
"""
from __future__ import annotations

import argparse
import glob
import json
import os
import re
import subprocess
import sys
from datetime import datetime

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
RESULTS = os.path.join(ROOT, "bench-results")
GOLDEN = os.path.join(ROOT, "docs", "perf-numbers.json")

# 官网（独立仓库 gox-website）不放 bench-results，它只引用 golden：把本脚本复制过去
# 当生成器用，用 --golden 指向主仓的 docs/perf-numbers.json、--root 指向官网仓根。
# 这样「三处数字一致」不是靠人工比对，而是同一份 golden 的两次渲染。当 CI 拿不到
# 主仓工作树时，还会回退去 GitHub raw 取同一份文件（见 fetch_golden）。

# 需要维护数字块的文档（相对仓库根）。
# 主仓：README 两份。官网仓（gox-website）由 --files 指定（guide/performance.md 等）。
DOC_FILES = ["README.md", "README.en.md"]

# 「三处一致」里的三处：README.md / README.en.md / 官网 guide/performance.md(+en)。
# 官网块 id 与 README 同名（readme-quad 等），因为内容形状一样，只是所在文件不同。
SITE_BLOCKS = ("readme-quad", "readme-quad-en", "readme-conclusion", "readme-conclusion-en")

# 「表格行内的数字」不能像段落那样用注释标记包起来：markdown 表格是连续的块，
# 中间插一行 `<!-- ... -->` 会把表格截断成两张（VitePress 里直接渲染坏）。
# 所以这一种块改成**整行占位符**：文档里留一行唯一的前缀，脚本按行替换。
ROW_PREFIX = "PERF-NUMBERS-ROW:"

# 「与 goja 同级」阈值（docs/performance.md §3.1），本脚本只引用、不重定义。
GOJA_SAME_CLASS = 1.25

MARK_BEGIN = "<!-- perf-numbers:begin {} -->"
MARK_END = "<!-- perf-numbers:end {} -->"

# 四栈展示顺序：同类基线（goja）紧随 gox，JIT 栈放后面并标注。
STACK_ORDER = ["gox", "goja", "bun", "node"]
STACK_LABEL = {
    "gox": "gox",
    "goja": "goja（纯解释器对照）",
    "node": "node / V8（JIT 对照）",
    "bun": "bun（JSC/JIT 对照）",
}
STACK_LABEL_EN = {
    "gox": "gox",
    "goja": "goja (pure interpreter)",
    "node": "node / V8 (JIT)",
    "bun": "bun (JSC/JIT)",
}


# ─────────────────────────────────────────────────────────────────────────────
# 读原始结果 → golden
# ─────────────────────────────────────────────────────────────────────────────
def _latest(pattern: str) -> str | None:
    """按文件名里嵌的 YYYYMMDD-HHMMSS 时间戳挑最新，**不按字典序**。

    为什么不能直接 sorted(glob)：`bench-*.json` 会顺带匹配到
    `bench-T2-4-baseline.json`、`bench-2026...` 之外的手工存档（字典序里
    "T" > "2"，baseline 反而排最后），曾因此把 2026-10-28 的旧数据当成最新。
    所以先按严格命名 `bench-YYYYMMDD-HHMMSS.json` 过滤，再取时间戳最大。
    """
    files = glob.glob(os.path.join(RESULTS, pattern))
    stamped = []
    for p in files:
        m = re.search(r"-(\d{8})-(\d{6})\.json$", os.path.basename(p))
        if m:
            stamped.append((m.group(1) + m.group(2), p))
    if not stamped:
        return None
    return max(stamped)[1]


def _load(path: str) -> dict:
    with open(path, encoding="utf-8") as f:
        return json.load(f)


def _bench_commit(bench: dict, bench_path: str) -> str | None:
    """gox 的 commit：优先取结果 JSON 自带字段，其次问二进制，最后退回当前 git HEAD。

    为什么要问二进制：bench-compare.sh 里带 -ldflags 注入的构建才会把 commit
    留在结果里；旧的裸 `git rev-parse HEAD` 会在「未提交的改动」上给出错误的
    归属（工作树改了但 HEAD 没变）。优先级按可信度排。
    """
    b = bench.get("binaries") or {}
    for v in (bench.get("gox_commit"), b.get("commit")):
        if v:
            return v
    bin_path = os.path.join(ROOT, "bench-results", "gox-bench")
    if os.path.exists(bin_path):
        try:
            out = subprocess.run([bin_path, "version"], capture_output=True, text=True, timeout=10).stdout
            m = re.search(r"^commit\s+([0-9a-f]{7,40})(\s+\(dirty\))?", out, re.M)
            if m:
                return m.group(1) + ("+dirty" if m.group(2) else "")
        except (OSError, subprocess.SubprocessError):
            pass
    try:
        out = subprocess.run(
            ["git", "rev-parse", "HEAD"], cwd=ROOT, capture_output=True, text=True, timeout=10
        ).stdout.strip()
        return out or None
    except (OSError, subprocess.SubprocessError):
        return None


def _node_version() -> str | None:
    try:
        out = subprocess.run(["node", "--version"], capture_output=True, text=True, timeout=10).stdout.strip()
        return out or None
    except (OSError, subprocess.SubprocessError):
        return None


def record() -> dict:
    """从 bench-results 里最新的 bench-*.json 与 coldstart-process-*.json 提炼 golden。"""
    bench_path = _latest("bench-*.json")
    if not bench_path:
        sys.exit("找不到 bench-results/bench-*.json，先跑 scripts/bench-compare.sh")
    bench = _load(bench_path)

    # 只取「参与对比」的栈：bun 在不装的机器上缺失是正常情况，格式上必须允许缺列。
    # 注意字段名是 `results`（早期草案叫 `stacks`），两个都认，避免换版本炸掉。
    raw = bench.get("results") or bench.get("stacks") or []
    stacks: dict[str, dict] = {}
    for s in raw:
        name = s.get("stack")
        if not name:
            continue
        stacks[name] = {
            "startup_ms": s.get("startup_ms"),
            "fib28_ms": s.get("fib28_ms"),
            "timers10k_ms": s.get("timers10k_ms"),
            "rss_kb": s.get("rss_kb"),
        }
    if "gox" not in stacks:
        sys.exit(f"{bench_path} 里没有 gox 栈，无法生成对外数字")

    host = bench.get("host", {})
    golden = {
        "_generated": "由 scripts/gen-perf-numbers.py --record 生成，勿手改",
        "source": {
            "bench": os.path.relpath(bench_path, ROOT),
            "generated_at": bench.get("generated_at"),
            "runs": bench.get("runs_per_metric") or bench.get("runs"),
            "host": {
                "cpu": host.get("cpu"),
                "cores": host.get("cores"),
                "os": host.get("platform"),
            },
            # 二进制自带的 commit/dirty 元数据（`gox version` 同源）：没它数字就不可追溯。
            "gox_commit": _bench_commit(bench, bench_path),
            "node_version": _node_version(),
        },
        "stacks": stacks,
    }

    cs_path = _latest("coldstart-process-*.json")
    if cs_path:
        cs = _load(cs_path)
        golden["coldstart"] = {
            "source": os.path.relpath(cs_path, ROOT),
            "first_frame_ms_median": cs.get("first_frame_ms_median"),
            "runs": cs.get("runs"),
            "display": (cs.get("host") or {}).get("display"),
        }
    return golden


# ─────────────────────────────────────────────────────────────────────────────
# golden → 文档块
# ─────────────────────────────────────────────────────────────────────────────
def _r(value) -> str:
    """四舍五入成文档里用的整数；缺失写 —（不许用 0 冒充）。"""
    if value is None:
        return "—"
    return str(int(round(value)))


def _mb(kb) -> str:
    if kb is None:
        return "—"
    return f"{round(kb / 1000, 1):g} MB"


def _ratio(num, den) -> float | None:
    if not num or not den:
        return None
    return num / den


def _fmt_ratio(x: float | None, digits: int = 2) -> str:
    return "—" if x is None else f"{x:.{digits}f}×"


def _gox_goja(g: dict) -> float | None:
    gox = g["stacks"].get("gox") or {}
    goja = g["stacks"].get("goja") or {}
    return _ratio(gox.get("fib28_ms"), goja.get("fib28_ms"))


def _gox_node(g: dict) -> float | None:
    gox = g["stacks"].get("gox") or {}
    node = g["stacks"].get("node") or {}
    return _ratio(gox.get("fib28_ms"), node.get("fib28_ms"))


def block_readme_quad(g: dict, lang: str) -> str:
    src = g["source"]
    host = src.get("host") or {}
    # 日期只取到秒：JSON 里带微秒，写进文档既吵又会让「同一批数据重跑」产生无意义的 diff。
    when = (src.get("generated_at") or "")[:19].replace("T", " ")
    names = [n for n in STACK_ORDER if n in g["stacks"]]
    labels = STACK_LABEL if lang == "zh" else STACK_LABEL_EN
    if lang == "zh":
        head = "| 指标 | " + " | ".join(labels.get(n, n) for n in names) + " |"
        sep = "|---|" + "".join("---:|" for _ in names)
        rows = [
            ("启动耗时", "startup_ms", _r),
            ("`fib28` 纯计算", "fib28_ms", _r),
            ("1 万定时器", "timers10k_ms", _r),
            ("常驻内存 RSS", "rss_kb", _mb),
        ]
        lines = [head, sep]
        for title, key, fmt in rows:
            cells = " | ".join(fmt((g["stacks"].get(n) or {}).get(key)) for n in names)
            lines.append(f"| {title} | {cells} |")
        note = (
            f"> 上表由 `scripts/gen-perf-numbers.py` 从 `{src['bench']}` 生成（**不要手改**）。\n"
            f"> 测量条件：{host.get('cpu')}（{host.get('cores')} 核）/ {host.get('os')}；"
            f"每项外部 wall time 跑 {src.get('runs')} 次取中位；日期 {when}（UTC）；"
            f"gox commit `{(src.get('gox_commit') or '未知')[:7]}`；node {src.get('node_version') or '未知'}。\n"
            f"> 复现命令见 `docs/performance.md` §9。"
        )
    else:
        head = "| Metric | " + " | ".join(labels.get(n, n) for n in names) + " |"
        sep = "|---|" + "".join("---:|" for _ in names)
        rows_en = [
            ("Startup", "startup_ms", _r),
            ("`fib28` pure compute", "fib28_ms", _r),
            ("10k timers", "timers10k_ms", _r),
            ("Idle RSS", "rss_kb", _mb),
        ]
        lines = [head, sep]
        for title, key, fmt in rows_en:
            cells = " | ".join(fmt((g["stacks"].get(n) or {}).get(key)) for n in names)
            lines.append(f"| {title} | {cells} |")
        note = (
            f"> Generated by `scripts/gen-perf-numbers.py` from `{src['bench']}` (**do not hand-edit**).\n"
            f"> Conditions: {host.get('cpu')} ({host.get('cores')} cores) / {host.get('os')}; "
            f"external wall time, median of {src.get('runs')} runs; date {when} (UTC); "
            f"gox commit `{(src.get('gox_commit') or 'unknown')[:7]}`; node {src.get('node_version') or 'unknown'}.\n"
            f"> Repro commands: `docs/performance.md` §9."
        )
    return "\n".join(lines) + "\n\n" + note


def _verdicts(g: dict) -> dict:
    gg = _gox_goja(g)
    gn = _gox_node(g)
    return {
        "gox_goja": gg,
        "gox_node": gn,
        "goja_ok": gg is not None and gg <= GOJA_SAME_CLASS,
        "node_ok": gn is not None and gn <= 3.0,
    }


def block_readme_conclusion(g: dict, lang: str) -> str:
    v = _verdicts(g)
    gg = _fmt_ratio(v["gox_goja"])
    gn = _fmt_ratio(v["gox_node"])
    thr = f"{GOJA_SAME_CLASS:g}×"
    if lang == "zh":
        if v["goja_ok"]:
            goja_line = f"**纯计算算力与 goja（同类纯解释器）同级**：`gox_fib28 / goja_fib28` = {gg} ≤ {thr}（docs §3.1 阈值）"
        else:
            goja_line = f"**纯计算相对 goja 未达同级阈值**：`gox_fib28 / goja_fib28` = {gg} > {thr}（docs §3.1），计入未达标项"
        if v["node_ok"]:
            node_line = f"**对 node 的 3× 红线本轮达标**：`gox_fib28 / node_fib28` = {gn}（原值口径）"
        else:
            node_line = (
                f"**对 node 的 3× 红线未达标**：`gox_fib28 / node_fib28` = {gn}（原值口径），"
                "且 node 列受进程启动噪声主导；按纯计算口径差距是数十倍量级 —— 这是「纯解释器 vs JIT」的固有差距"
            )
        return "\n".join(
            [
                f"- {goja_line}。",
                f"- {node_line}；gox 刻意不做 JIT，该红线在不开 JIT 的前提下结构上不可达（口径待重议）。",
                "- 与 bun/node 等 JIT 栈的 `fib28` 差距**不作为优化目标**，引用任何 bun/node 数字必须连同上方口径免责。",
            ]
        )
    if v["goja_ok"]:
        goja_line = f"**Pure-compute on par with goja (same-class pure interpreter)**: `gox_fib28 / goja_fib28` = {gg} ≤ {thr} (docs §3.1)"
    else:
        goja_line = f"**Below the same-class threshold vs goja**: `gox_fib28 / goja_fib28` = {gg} > {thr} (docs §3.1)"
    node_line = f"`gox_fib28 / node_fib28` = {gn} (raw wall)" + (" — within the 3× red line" if v["node_ok"] else " — above the 3× red line")
    return "\n".join(
        [
            f"- {goja_line}.",
            f"- Against node (raw wall): {node_line}. The gap is the inherent pure-interpreter-vs-JIT gap (gox deliberately has no JIT).",
            "- The `fib28` gap against bun/node JIT stacks is **not an optimization target**; any bun/node number must ship with the disclaimer above.",
        ]
    )


def block_readme_threshold(g: dict, lang: str) -> str:
    """README 前文那一行里的两个数字（纯计算倍数 + 日期）。"""
    v = _verdicts(g)
    gg = _fmt_ratio(v["gox_goja"])
    date = (g["source"].get("generated_at") or "")[:10]
    if lang == "zh":
        tail = "，落入同类阈值 ×1.25 内" if v["goja_ok"] else "，**超出同类阈值 ×1.25**"
        return f"**{gg}**（{date} 本机夜批复测{tail}）"
    tail = ", within the same-class ×1.25 threshold" if v["goja_ok"] else ", **above the same-class ×1.25 threshold**"
    return f"**{gg}** ({date} local nightly re-test{tail})"



def block_site_verdict_row(g: dict, lang: str = "zh") -> str:
    """官网「一句话结论」表里的那一行 —— 返回**不含换行**的单行内容。"""
    v = _verdicts(g)
    if lang == "en":
        gg_en = _fmt_ratio(v["gox_goja"])
        gox_en = (g["stacks"].get("gox") or {}).get("fib28_ms")
        goja_en = (g["stacks"].get("goja") or {}).get("fib28_ms")
        date_en = (g["source"].get("generated_at") or "")[:10]
        judged_en = "✅ **Passed**" if v["goja_ok"] else "❌ **Not met**"
        return (
            f"| Interpreter on par with goja | {judged_en} (re-test {date_en}) | "
            f"`fib28` gox **{_r(gox_en)}ms** vs goja **{_r(goja_en)}ms** → **{gg_en}** "
            f"{'≤' if v['goja_ok'] else '>'} same-class threshold ×{GOJA_SAME_CLASS:g} "
            "(history: 2.39× → 1.7× → 1.32× → 1.22×) |"
        )
    gg = _fmt_ratio(v["gox_goja"])
    gox = (g["stacks"].get("gox") or {}).get("fib28_ms")
    goja = (g["stacks"].get("goja") or {}).get("fib28_ms")
    date = (g["source"].get("generated_at") or "")[:10]
    judged = "✅ **达标**" if v["goja_ok"] else "❌ **未达标**"
    return (
        f"| 解释器算力达 goja 同级 | {judged}（{date} 复测） | "
        f"`fib28` gox **{_r(gox)}ms** vs goja **{_r(goja)}ms** → **{gg}** "
        f"{'≤' if v['goja_ok'] else '>'} 同类阈值 ×{GOJA_SAME_CLASS:g}（历史：2.39× → 1.7× → 1.32× → 1.22×） |"
    )

def block_site_verdict(g: dict) -> str:
    """官网「一句话结论」表的一行：解释器算力判定（表格行，不能带空行）。"""
    v = _verdicts(g)
    gg = _fmt_ratio(v["gox_goja"])
    gox = (g["stacks"].get("gox") or {}).get("fib28_ms")
    goja = (g["stacks"].get("goja") or {}).get("fib28_ms")
    date = (g["source"].get("generated_at") or "")[:10]
    judged = "✅ **达标**" if v["goja_ok"] else "❌ **未达标**"
    return (
        f"| 解释器算力达 goja 同级 | {judged}（{date} 复测） | "
        f"`fib28` gox **{_r(gox)}ms** vs goja **{_r(goja)}ms** → **{gg}** "
        f"{'≤' if v['goja_ok'] else '>'} 同类阈值 ×{GOJA_SAME_CLASS:g}（历史：2.39× → 1.7× → 1.32× → 1.22×） |"
    )


def block_site_pure_compute(g: dict, lang: str = "zh") -> str:
    """官网 §2.1「扣掉启动开销后」的表：官方口径是 wall − startup 的估算，只用于讲清噪声结构。

    注意：这张表**只做估算说明**，判定一律用纯 wall 的 gox/goja 比值（§2.2），因为
    startup 相减在个位数 ms 时误差与信号同量级 —— 表里必须写清这一点。
    """
    stacks = g["stacks"]
    gox = stacks.get("gox") or {}
    goja = stacks.get("goja") or {}
    node = stacks.get("node") or {}
    gox_c = (gox.get("fib28_ms") or 0) - (gox.get("startup_ms") or 0)
    goja_c = (goja.get("fib28_ms") or 0) - (goja.get("startup_ms") or 0)
    rel_goja = _ratio(goja_c, gox_c) if gox_c and goja_c else None
    goja_txt = (
        f"**{rel_goja:.2f}×**（goja {'快' if rel_goja and rel_goja < 1 else '慢'} "
        f"{abs(1 / rel_goja - 1) * 100:.0f}%，同类基线）"
        if rel_goja
        else "—"
    )
    if lang == "en":
        rel = f"**{rel_goja:.2f}×** (goja {'faster' if rel_goja < 1 else 'slower'}), same-class baseline" if rel_goja else "—"
        lines = [
            "| Stack | Estimated fib28 pure compute | Relative to gox |",
            "| --- | ---: | --- |",
            f"| gox | {_r(gox.get('fib28_ms'))} − {_r(gox.get('startup_ms'))} ≈ **{_r(gox_c)} ms** | 1.00× |",
            f"| goja | {_r(goja.get('fib28_ms'))} − {_r(goja.get('startup_ms'))} ≈ **{_r(goja_c)} ms** | {rel} |",
            f"| node / V8 | {_r(node.get('fib28_ms'))} − {_r(node.get('startup_ms'))} ≈ "
            f"**{_r((node.get('fib28_ms') or 0) - (node.get('startup_ms') or 0))} ms** | inherent JIT gap, not a target |",
        ]
        note = (
            f"> Estimate basis: `fib28_ms − startup_ms`, same data as `{g['source']['bench']}`.\n"
            "> **The verdict is not based on this table**: startup is single-digit ms, so the subtraction "
            "error is the same order as the signal; the verdict always uses the pure wall ratio of §2.2. "
            "node's estimate is also affected by `node -e` vs file loading — magnitude reference only."
        )
        return "\n".join(lines) + "\n\n" + note

    lines = [
        "| 栈 | fib28 纯计算估算 | 相对 gox |",
        "| --- | ---: | --- |",
        f"| gox | {_r(gox.get('fib28_ms'))} − {_r(gox.get('startup_ms'))} ≈ **{_r(gox_c)} ms** | 1.00× |",
        f"| goja | {_r(goja.get('fib28_ms'))} − {_r(goja.get('startup_ms'))} ≈ **{_r(goja_c)} ms** | {goja_txt} |",
        f"| node / V8 | {_r(node.get('fib28_ms'))} − {_r(node.get('startup_ms'))} ≈ "
        f"**{_r((node.get('fib28_ms') or 0) - (node.get('startup_ms') or 0))} ms** | "
        "JIT 固有差距，不作为目标 |",
    ]
    note = (
        f"> 估算口径：`fib28_ms − startup_ms`，数据同 `{g['source']['bench']}`。\n"
        "> **判定不用这张表**：startup 只有个位数 ms，相减的误差与信号同量级；判定一律用 §2.2 的"
        "纯 wall 比值。node 的纯计算估计受 `node -e` 与文件加载差异影响，只作量级参考。"
    )
    # 表格块必须首行即表格，脚注与表格之间留空行
    return "\n".join(lines) + "\n\n" + note


# 表格行块：整行占位（见 ROW_PREFIX）
ROW_BLOCKS = {
    "site-verdict-row": block_site_verdict_row,
    "site-verdict-row-en": lambda g: block_site_verdict_row(g, "en"),
}

BLOCKS = {
    "readme-quad": lambda g: block_readme_quad(g, "zh"),
    "readme-quad-en": lambda g: block_readme_quad(g, "en"),
    "readme-conclusion": lambda g: block_readme_conclusion(g, "zh"),
    "readme-conclusion-en": lambda g: block_readme_conclusion(g, "en"),
    "readme-threshold": lambda g: block_readme_threshold(g, "zh"),
    "readme-threshold-en": lambda g: block_readme_threshold(g, "en"),
    "site-verdict-row": block_site_verdict_row,
    "site-verdict-row-en": lambda g: block_site_verdict_row(g, "en"),
    "site-pure-compute": block_site_pure_compute,
    "site-pure-compute-en": lambda g: block_site_pure_compute(g, "en"),
}


# ─────────────────────────────────────────────────────────────────────────────
# 文档块替换 / 校验
# ─────────────────────────────────────────────────────────────────────────────
def _merge(body: str, inner: str) -> str:
    """把新内容插进块内，保留块内原有的首尾空行风格（markdown 渲染必需）。"""
    return "\n" + inner.strip("\n") + "\n"


def rewrite(text: str, block_id: str, inner: str) -> tuple[str, int]:
    begin, end = MARK_BEGIN.format(block_id), MARK_END.format(block_id)
    pattern = re.compile(re.escape(begin) + r".*?" + re.escape(end), re.S)
    matches = pattern.findall(text)
    if len(matches) != 1:
        return text, len(matches)
    new = begin + _merge(text, inner) + end
    return pattern.sub(lambda _m: new, text, count=1), 1


def rewrite_row(text: str, block_id: str, content: str) -> tuple[str, int]:
    """整行占位符替换：匹配以 `PREFIX` + id 开头的整行，替换成 content（单行）。

    为什么单独一套：见 ROW_PREFIX 的说明 —— 表格中间不能塞注释。
    """
    needle = f"{ROW_PREFIX}{block_id}"
    # 必须钉住行尾：`site-verdict-row` 是 `site-verdict-row-en` 的前缀，不钉住行尾会
    # 把英文文件的占位误当成中文行块来渲染（曾在英文页出现中文行）。
    pattern = re.compile(r"^[ \t]*" + re.escape(needle) + r"[ \t]*$", re.M)
    hits = len(pattern.findall(text))
    if hits != 1:
        return text, hits
    return pattern.sub(lambda _m: content, text, count=1), 1


def iter_docs(extra: list[str] | None = None) -> list[str]:
    out = []
    for name in list(DOC_FILES) + list(extra or []):
        p = name if os.path.isabs(name) else os.path.join(ROOT, name)
        if os.path.exists(p):
            out.append(p)
    return out


def fetch_golden(url: str) -> dict:
    """从 GitHub raw 取 golden（官网 CI 没有主仓工作树时的兜底）。

    为什么允许联网取：官网仓与主仓是两个仓库，官网构建**唯一**需要的跨仓输入就是
    这份 golden。与其让两边各存一份 JSON（那又回到手抄），不如让官网在构建时拉
    主仓的那一份 —— 主仓一变，官网下次构建自动跟上。
    """
    import urllib.request

    try:
        with urllib.request.urlopen(url, timeout=15) as resp:  # noqa: S310 - 固定 https 地址，来自本仓 CI 配置
            return json.loads(resp.read().decode("utf-8"))
    except (OSError, ValueError) as exc:
        print(f"::error::取 golden 失败（{url}）：{exc}", file=sys.stderr)
        raise SystemExit(2) from exc


def cmd_write(g: dict, extra: list[str] | None = None) -> int:
    changed = 0
    for path in iter_docs(extra):
        with open(path, encoding="utf-8") as f:
            text = f.read()
        original = text
        for block_id, fn in ROW_BLOCKS.items():
            if f"{ROW_PREFIX}{block_id}" not in text:
                continue
            text, hits = rewrite_row(text, block_id, fn(g))
            if hits > 1:
                print(f"::error::{path} 里占位 {block_id} 出现了 {hits} 次", file=sys.stderr)
                return 1
        for block_id, fn in BLOCKS.items():
            text, hits = rewrite(text, block_id, fn(g))
            # 文档里没放这个块是允许的（英文版可以只有 -en 块），放了就必须唯一。
            if hits > 1:
                print(f"::error::{path} 里标记 {block_id} 出现了 {hits} 次", file=sys.stderr)
                return 1
        if text != original:
            with open(path, "w", encoding="utf-8") as f:
                f.write(text)
            changed += 1
            print(f"已更新 {os.path.relpath(path, ROOT)}")
    print(f"完成：{changed} 个文件有变化")
    return 0


def cmd_check(g: dict, extra: list[str] | None = None) -> int:
    bad = 0
    seen = 0
    for path in iter_docs(extra):
        with open(path, encoding="utf-8") as f:
            text = f.read()
        for block_id, fn in ROW_BLOCKS.items():
            if f"{ROW_PREFIX}{block_id}" not in text:
                continue
            seen += 1
            _, hits = rewrite_row(text, block_id, fn(g))
            if hits != 1:
                print(f"::error::{os.path.relpath(path, ROOT)} 占位 {block_id} 出现 {hits} 次（必须恰好 1 次）")
                bad += 1
                continue
            if rewrite_row(text, block_id, fn(g))[0] != text:
                print(f"::error::{os.path.relpath(path, ROOT)} 的表格行 {block_id} 与 golden 不一致 —— 别手改这一行")
                bad += 1
        for block_id, fn in BLOCKS.items():
            begin, end = MARK_BEGIN.format(block_id), MARK_END.format(block_id)
            if begin not in text:
                continue
            seen += 1
            _, hits = rewrite(text, block_id, fn(g))
            if hits != 1:
                print(f"::error::{os.path.relpath(path, ROOT)} 标记 {block_id} 出现 {hits} 次（必须恰好 1 次）")
                bad += 1
                continue
            rewritten, _ = rewrite(text, block_id, fn(g))
            if rewritten != text:
                print(f"::error::{os.path.relpath(path, ROOT)} 的数字块 {block_id} 与 golden 不一致 —— 跑 --write 重新生成，别手改")
                bad += 1
    if not seen:
        print("::error::没有任何文档块，检查标记是否被误删", file=sys.stderr)
        return 1
    if bad:
        return 1
    print(f"ok  数字块与 golden 一致（{seen} 个块）")
    return 0


def main(argv: list[str]) -> int:
    global ROOT, GOLDEN
    ap = argparse.ArgumentParser(description="性能对外数字单一真相源")
    ap.add_argument("--record", action="store_true", help="从最新 bench-results 重建 golden")
    ap.add_argument("--write", action="store_true", help="用 golden 重写文档数字块")
    ap.add_argument("--check", action="store_true", help="校验文档数字块与 golden 一致")
    ap.add_argument("--print", dest="show", metavar="ID", help="打印指定块内容")
    ap.add_argument("--root", default=ROOT, help="仓库根（官网仓用它指向自己的根）")
    ap.add_argument("--golden", default=GOLDEN, help="golden 路径（官网仓指向主仓的 perf-numbers.json）")
    ap.add_argument("--golden-url", help="golden 的 raw URL（--golden 不存在时回退拉取）")
    ap.add_argument("--files", nargs="*", default=[], help="额外要处理的文档（相对 --root 或绝对路径）")
    args = ap.parse_args(argv)

    ROOT = os.path.abspath(args.root)
    GOLDEN = args.golden

    if args.record:
        g = record()
        with open(GOLDEN, "w", encoding="utf-8") as f:
            json.dump(g, f, ensure_ascii=False, indent=2)
            f.write("\n")
        print(f"已写入 {os.path.relpath(GOLDEN, ROOT)}（源：{g['source']['bench']}）")
        return 0

    extra = list(args.files)
    if not os.path.exists(GOLDEN):
        if args.golden_url:
            g = fetch_golden(args.golden_url)
        else:
            print("::error::docs/perf-numbers.json 不存在，先跑 --record", file=sys.stderr)
            return 2
    else:
        g = _load(GOLDEN)

    if args.show:
        fn = BLOCKS.get(args.show)
        if not fn:
            print(f"未知块 id：{args.show}（可用：{', '.join(BLOCKS)}）", file=sys.stderr)
            return 2
        print(fn(g))
        return 0

    if args.write:
        return cmd_write(g, extra)
    if args.check:
        return cmd_check(g, extra)
    ap.print_help()
    return 2


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
