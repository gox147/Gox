#!/usr/bin/env python3
"""冷启动回归闸门 (T3-5) —— 与 check-bench.py 同族，但只盯"启动"。

背景：check-bench.py 的 8 项判据全是**算力**（fib28 / timers / rss / startup
的跨栈比值）。冷启动是另一条线：优化它的人看 check-bench 永远是绿的，于是
"首帧变慢 20%" 没有任何东西会拦。2026-11-17 夜批修复了字体栈里两处重复读盘
（19MB 字体在一次启动里被读 2~3 遍），一次省下 ~25ms；这类改动必须有闸门，
否则下一次同样的浪费会悄悄回来。

## 三条判据（分别对应三个口径，**不可互相替代**）

| 判据 | 数据来源 | 作用 |
|---|---|---|
| `font_stage_total_ms` | `go test ./gfx/ -run TestColdstartFontStage` | 首次用字体的两段（目录扫描 + 选默认字体）合计。**最贴近根因**：引擎在启动期自己做了什么 |
| `first_frame_ms`（进程级） | `bash scripts/coldstart.sh` | 用户双击 exe 到首帧的体感（含窗口/字体/上屏）。**对外口径** |
| `first_frame_ms`（进程内） | `go test ./tools/perf/ -run TestColdStartToFirstFrame` | CI 无显示环境也能跑的回归盯梢 |

## 为什么不在 CI 上硬判毫秒

字体扫描的成本 ≈ 字体目录的**字节数**（本机 330 个文件 / 148MB ≈ 36–50ms），
而这个数字随 runner 镜像变化（装没装 fonts-noto-cjk 差好几倍）。所以：

- **有基线可比就按比值判**，阈值 1.25（与 check-bench 的 fib28 同档）；
- **没有基线**时只按**绝对上限**判红（明显病态的慢），并提示用
  `--update-baseline` 固化；
- host 指纹（platform / cpu / cores）与基线不一致时，进程级首帧降级为
  "只记录"—— 换机器后绝对值不可比，这一点与 check-bench 的 startup/rss 同款。

## 用法

    python3 scripts/check-coldstart.py                  # 自动找 bench-results 里最新的三份
    python3 scripts/check-coldstart.py --json <file>... # 指定文件
    python3 scripts/check-coldstart.py --update-baseline
    python3 scripts/check-coldstart.py --self-test      # 负向自测（闸门自检）

退出码：0 = 通过（或只有记录项）；1 = 存在回归；2 = 用法/数据错误。
"""
from __future__ import annotations

import argparse
import glob
import json
import os
import platform
import sys

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
RESULTS = os.path.join(ROOT, "bench-results")
BASELINE = os.path.join(RESULTS, "coldstart-baseline.json")

# 指标 → (允许的最大倍数, 没有基线时的绝对上限 ms, 说明)
LIMITS = {
    "font_stage_total_ms": (1.25, 120.0, "首次用字体的扫描+选默认合计（gfx TestColdstartFontStage）"),
    "process_first_frame_ms": (1.25, 400.0, "进程级冷启动到首帧（scripts/coldstart.sh，含窗口/上屏）"),
    "inproc_first_frame_ms": (1.25, 8.0, "进程内首帧（tools/perf，真窗口替身 Surface）"),
}

# 跨机型只记录、不判红的指标：进程级首帧含字体栈与显示栈，换机后绝对值不可比。
CROSS_HOST_RECORD_ONLY = {"process_first_frame_ms"}


def die(msg: str) -> None:
    print("coldstart-gate: %s" % msg)
    sys.exit(2)


def load(path: str) -> dict:
    try:
        with open(path, encoding="utf-8") as f:
            return json.load(f)
    except (OSError, ValueError) as e:
        die("读不了 %s：%s" % (path, e))
    return {}


def num(v):
    """提取一个正的有限数值；不是数字就返回 None（该项按"缺数据"处理）。"""
    if isinstance(v, bool):
        return None
    if isinstance(v, (int, float)):
        return float(v) if v > 0 else None
    return None


def kind_of(doc: dict) -> str:
    """按文件内容判定口径，而不是按文件名 —— 文件名可以被随手改。"""
    bench = str(doc.get("benchmark", ""))
    if "ColdstartFontStage" in bench:
        return "font"
    if "coldstart-process" in bench:
        return "process"
    if "ColdStartToFirstFrame" in bench:
        return "inproc"
    return ""


def extract(doc: dict) -> dict:
    """把一份结果文件映射成 {指标: 毫秒}。缺项留空，不猜。"""
    k = kind_of(doc)
    if k == "font":
        ph = doc.get("phases_ms") or {}
        total = num(ph.get("total_ms"))
        if total is None:
            scan, choose = num(ph.get("scan_ms")), num(ph.get("choose_ms"))
            total = (scan or 0) + (choose or 0) if (scan or choose) else None
        return {"font_stage_total_ms": total}
    if k == "process":
        return {"process_first_frame_ms": num(doc.get("first_frame_ms_median"))}
    if k == "inproc":
        ph = doc.get("phases_ms") or {}
        return {"inproc_first_frame_ms": num(ph.get("first_frame_ms"))}
    return {}


def host_of(doc: dict) -> str:
    h = doc.get("host") or {}
    # 三种结果文件的 host 字段形状不同（generator 各写各的），统一取三项。
    cpu = h.get("cpu") or doc.get("platform", {}).get("cpu") or ""
    cores = h.get("cores") or doc.get("platform", {}).get("num_cpu") or ""
    plat = h.get("platform") or ""
    if not plat:
        plat = "%s/%s" % (platform.system(), platform.machine())
    return "%s|%s|%s" % (plat, cpu, cores)


def collect(paths: list[str]) -> tuple[dict, dict]:
    """返回 (指标值, 指标来源文件)。同指标多份时取**最新**（按 generated_at）。"""
    values: dict[str, float] = {}
    from_path: dict[str, str] = {}
    stamps: dict[str, str] = {}
    for p in paths:
        doc = load(p)
        stamp = str(doc.get("generated_at", ""))
        for metric, v in extract(doc).items():
            if v is None:
                continue
            if metric in stamps and stamp < stamps[metric]:
                continue
            values[metric] = v
            stamps[metric] = stamp
            from_path[metric] = p
    return values, from_path


def auto_paths() -> list[str]:
    pats = [
        "coldstart-font-*.json",
        "coldstart-process-*.json",
        "coldstart-2*.json",  # 进程内口径: coldstart-<UTC>.json（不加前缀, 故用年份数字区分）
    ]
    out = []
    for pat in pats:
        out.extend(sorted(glob.glob(os.path.join(RESULTS, pat))))
    return out


def build_baseline(values: dict, hosts: dict) -> dict:
    return {
        "说明": "冷启动基线 —— 由 python3 scripts/check-coldstart.py --update-baseline 生成。"
                "刻意优化过启动（或确认是有意的变化）之后才重建；平时只比对不自动刷新。",
        "生成方式": "python3 scripts/check-coldstart.py --update-baseline",
        "指标": values,
        "host": hosts,
    }


def compare(base: dict, values: dict, hosts: dict) -> tuple[list, list]:
    """返回 (回归列表, 记录列表)。每条是 (指标, 文本)。"""
    fails, notes = [], []
    base_vals = (base or {}).get("指标", {}) or {}
    base_host = (base or {}).get("host", {}) or {}
    for metric, (ratio_max, abs_max, desc) in LIMITS.items():
        cur = values.get(metric)
        if cur is None:
            notes.append((metric, "本次结果里没有这一项（对应脚本没跑/被跳过），跳过判定"))
            continue
        b = base_vals.get(metric)
        if b:
            r = cur / float(b)
            cross = base_host.get(metric) and base_host.get(metric) != hosts.get(metric)
            if cross and metric in CROSS_HOST_RECORD_ONLY:
                notes.append((metric, "host 与基线不一致（%s vs %s），绝对值不可比，只记录：%.2fms（基线 %.2fms）"
                              % (hosts.get(metric), base_host.get(metric), cur, float(b))))
                continue
            if r > ratio_max:
                fails.append((metric, "%.2fms 是基线 %.2fms 的 %.2fx，超过 %.2fx（%s）"
                              % (cur, float(b), r, ratio_max, desc)))
            else:
                notes.append((metric, "%.2fms（基线 %.2fms，%.2fx ≤ %.2fx）" % (cur, float(b), r, ratio_max)))
            continue
        # 没有基线：只拦明显病态的慢
        if cur > abs_max:
            fails.append((metric, "%.2fms 超过绝对上限 %.0fms（%s），且还没有基线可比 —— "
                                  "先确认机器负载，再跑 --update-baseline" % (cur, abs_max, desc)))
        else:
            notes.append((metric, "%.2fms（无基线，仅按 %.0fms 绝对上限判定）" % (cur, abs_max)))
    return fails, notes


def print_report(fails, notes):
    print("| 指标 | 现状 |")
    print("|---|---|")
    for metric, text in notes:
        print("| %s | %s |" % (metric, text))
    for metric, text in fails:
        print("| %s | ✗ %s |" % (metric, text))
    print()
    if fails:
        print("coldstart-gate: %d 项回归" % len(fails))
        for metric, text in fails:
            print("  ✗ %s: %s" % (metric, text))
        return 1
    print("ok  冷启动无回归（%d 项判据）" % len(LIMITS))
    return 0


def self_test() -> int:
    """负向自测：一个从没失败过的闸门等于没有闸门。

    合成三份文档，验证 ① 未超阈值 → 绿；② 超过比值阈值 → 红；③ 无基线但
    超过绝对上限 → 红；④ 跨机型的进程级首帧 → 只记录。
    """
    ok = True

    def check(name, cond):
        nonlocal ok
        if not cond:
            ok = False
            print("  FAIL %s" % name)
        else:
            print("  ok   %s" % name)

    base = {"指标": {"font_stage_total_ms": 40.0, "process_first_frame_ms": 90.0,
                    "inproc_first_frame_ms": 0.7},
            "host": {"process_first_frame_ms": "linux|X|32"}}
    hosts = {"process_first_frame_ms": "linux|X|32"}

    f, _ = compare(base, {"font_stage_total_ms": 42.0, "process_first_frame_ms": 95.0,
                          "inproc_first_frame_ms": 0.72}, hosts)
    check("阈值内应绿", not f)

    f, _ = compare(base, {"font_stage_total_ms": 60.0}, hosts)
    check("扫描耗时 +50% 应红", any(m == "font_stage_total_ms" for m, _ in f))

    f, _ = compare({}, {"font_stage_total_ms": 500.0}, {})
    check("无基线且超绝对上限应红", any(m == "font_stage_total_ms" for m, _ in f))

    f, n = compare(base, {"process_first_frame_ms": 300.0},
                   {"process_first_frame_ms": "darwin|Y|8"})
    check("跨机型进程级首帧只记录不判红", not f and any(m == "process_first_frame_ms" for m, _ in n))

    # 解析层：三条口径必须各自映射到正确的指标（文件名/字段写错会静默返回空）。
    doc_font = {"benchmark": "gfx/TestColdstartFontStage", "phases_ms": {"scan_ms": 30, "choose_ms": 1}}
    check("扫描+选默认缺 total 时由两项相加", extract(doc_font) == {"font_stage_total_ms": 31.0})
    doc_proc = {"benchmark": "coldstart-process", "first_frame_ms_median": 91}
    check("进程级取 median",
          extract(doc_proc) == {"process_first_frame_ms": 91.0})
    doc_in = {"benchmark": "perf/ColdStartToFirstFrame", "phases_ms": {"first_frame_ms": 0.74}}
    check("进程内取 phases_ms.first_frame_ms",
          extract(doc_in) == {"inproc_first_frame_ms": 0.74})
    check("未知口径返回空", extract({"benchmark": "something-else"}) == {})

    print("ok  冷启动闸门自测通过" if ok else "coldstart-gate: 自测失败")
    return 0 if ok else 1


def main() -> int:
    ap = argparse.ArgumentParser(add_help=True)
    ap.add_argument("--json", nargs="*", default=None, help="指定结果文件（缺省自动找最新的三份）")
    ap.add_argument("--update-baseline", action="store_true", help="把本次结果写成新基线")
    ap.add_argument("--self-test", action="store_true", help="负向自测")
    args = ap.parse_args()

    if args.self_test:
        return self_test()

    paths = args.json if args.json else auto_paths()
    if not paths:
        die("bench-results/ 下没有冷启动结果 —— 先跑 TestColdstartFontStage / coldstart.sh / TestColdStartToFirstFrame")
    values, from_path = collect(paths)
    if not values:
        die("给定的结果文件里没有任何冷启动字段：%s" % ", ".join(paths))

    hosts = {}
    for p in from_path.values():
        doc = load(p)
        k = kind_of(doc)
        if k == "process":
            hosts["process_first_frame_ms"] = host_of(doc)
        elif k == "font":
            hosts["font_stage_total_ms"] = host_of(doc)
        elif k == "inproc":
            hosts["inproc_first_frame_ms"] = host_of(doc)

    for metric, p in sorted(from_path.items()):
        print("数据: %s ← %s" % (metric, os.path.relpath(p, ROOT)))

    if args.update_baseline:
        with open(BASELINE, "w", encoding="utf-8") as f:
            json.dump(build_baseline(values, hosts), f, ensure_ascii=False, indent=2)
            f.write("\n")
        print("已写入基线 %s" % os.path.relpath(BASELINE, ROOT))
        return 0

    base = {}
    if os.path.exists(BASELINE):
        base = load(BASELINE)
    else:
        print("（没有 %s：本次只按绝对上限判定，确认无误后跑 --update-baseline 固化）"
              % os.path.relpath(BASELINE, ROOT))
    fails, notes = compare(base, values, hosts)
    return print_report(fails, notes)


if __name__ == "__main__":
    sys.exit(main())
