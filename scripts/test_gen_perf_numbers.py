"""gen-perf-numbers.py 的自测 —— 保护「数字单一真相源」闸门本身（看板 rrIdLN）。

用 python3 -m unittest scripts/test_gen_perf_numbers.py 或
python3 scripts/test_gen_perf_numbers.py 跑。

为什么要有这些用例：`--check` 只有在「块被手改」时才会红。如果脚本自身的块解析/
重写出错（比如正则吞掉标记、块缺首行空行导致 markdown 表格粘连），`--check` 会
永远输出 ok —— 一个永远绿的闸门比没有闸门更糟（check-bench.py 的 --self-test
是同一个道理，这里复用）。
"""
from __future__ import annotations

import importlib.util
import os
import sys
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
_spec = importlib.util.spec_from_file_location("gen_perf_numbers", os.path.join(HERE, "gen-perf-numbers.py"))
gpn = importlib.util.module_from_spec(_spec)
assert _spec.loader is not None
_spec.loader.exec_module(gpn)


def _golden(gox_fib=263, goja_fib=256, node_fib=46, bun_fib=None) -> dict:
    stacks = {
        "gox": {"startup_ms": 9, "fib28_ms": gox_fib, "timers10k_ms": 189, "rss_kb": 16112},
        "goja": {"startup_ms": 6, "fib28_ms": goja_fib, "timers10k_ms": 129, "rss_kb": 7540},
        "node": {"startup_ms": 36, "fib28_ms": node_fib, "timers10k_ms": 153, "rss_kb": 46572},
    }
    if bun_fib is not None:
        stacks["bun"] = {"startup_ms": 8, "fib28_ms": bun_fib, "timers10k_ms": 113, "rss_kb": 15524}
    return {
        "source": {
            "bench": "bench-results/bench-20261117-120235.json",
            "generated_at": "2026-11-17T12:02:35.314437+00:00",
            "runs": 5,
            "host": {"cpu": "TestCPU", "cores": 8, "os": "Linux-test"},
            "gox_commit": "b2a6730934e997d7d0940a760279b43cd2510408",
            "node_version": "v22.13.1",
        },
        "stacks": stacks,
    }


class TableTest(unittest.TestCase):
    def test_zh_table_shape(self):
        out = gpn.block_readme_quad(_golden(), "zh")
        lines = out.splitlines()
        self.assertTrue(lines[0].startswith("| 指标 | gox | goja"))
        self.assertIn("| `fib28` 纯计算 | 263 | 256 | 46 |", out)
        # JIT 栈必须带括号标注，否则读者会把 JIT 数字当同类基线
        self.assertIn("node / V8（JIT 对照）", lines[0])
        self.assertIn("（纯解释器对照）", lines[0])

    def test_en_table_shape(self):
        out = gpn.block_readme_quad(_golden(), "en")
        self.assertIn("| `fib28` pure compute | 263 | 256 | 46 |", out)
        self.assertIn("node / V8 (JIT)", out)

    def test_bun_column_only_when_present(self):
        """机器上没装 bun 时不得凭空造一列（缺失是常态，不许用 0 或旧值顶替）。"""
        with_bun = gpn.block_readme_quad(_golden(bun_fib=20), "zh")
        self.assertIn("bun（JSC/JIT 对照）", with_bun)
        without = gpn.block_readme_quad(_golden(), "zh")
        self.assertNotIn("bun", without)

    def test_missing_stack_cell_is_dash(self):
        g = _golden()
        g["stacks"]["goja"]["timers10k_ms"] = None
        out = gpn.block_readme_quad(g, "zh")
        self.assertIn("| 1 万定时器 | 189 | — | 153 |", out)

    def test_provenance_in_note(self):
        """数字必须能追溯：出处文件、commit、node 版本、日期都得写进脚注。"""
        out = gpn.block_readme_quad(_golden(), "zh")
        self.assertIn("bench-20261117-120235.json", out)
        self.assertIn("b2a6730", out)  # commit 只显示短号
        self.assertIn("v22.13.1", out)
        self.assertIn("2026-11-17 12:02:35", out)
        self.assertNotIn(".314437", out, "微秒精度会让同批数据重跑产生无意义 diff")


class ConclusionTest(unittest.TestCase):
    def test_pass_goja(self):
        out = gpn.block_readme_conclusion(_golden(), "zh")
        self.assertIn("同级", out)
        self.assertIn("1.03×", out)
        self.assertIn("5.72×", out)  # 263/46

    def test_fail_goja(self):
        out = gpn.block_readme_conclusion(_golden(gox_fib=400, goja_fib=256), "zh")
        self.assertIn("未达同级阈值", out)
        self.assertIn("1.56×", out)

    def test_node_red_line_status_follows_numbers(self):
        """3× 红线判据必须由数字决定，不能写死「未达标」三个字。"""
        passed = gpn.block_readme_conclusion(_golden(gox_fib=100, goja_fib=90, node_fib=46), "zh")
        self.assertIn("达标", passed)
        self.assertNotIn("未达标", passed)
        failed = gpn.block_readme_conclusion(_golden(gox_fib=300, goja_fib=256, node_fib=46), "zh")
        self.assertIn("未达标", failed)
        # 免责说明必须一直在：对外引用 node/bun 数字的前置条件
        self.assertIn("纯解释器 vs JIT", failed)

    def test_disclaimer_mandatory_en(self):
        out = gpn.block_readme_conclusion(_golden(), "en")
        self.assertIn("pure-interpreter-vs-JIT", out)
        self.assertIn("disclaimer", out)

    def test_threshold_block(self):
        self.assertIn("1.03×", gpn.block_readme_threshold(_golden(), "zh"))
        self.assertIn("×1.25", gpn.block_readme_threshold(_golden(), "zh"))


class RewriteTest(unittest.TestCase):
    DOC = "前言\n\n<!-- perf-numbers:begin X -->\nold\n<!-- perf-numbers:end X -->\n\n后记\n"

    def test_rewrite_inserts_blank_lines(self):
        """块内首尾必须留空行：表格紧跟标记或紧跟后文会让 markdown 不渲染成表。"""
        out, hits = gpn.rewrite(self.DOC, "X", "| a | b |\n|---|---|")
        self.assertEqual(hits, 1)
        self.assertIn("<!-- perf-numbers:begin X -->\n| a | b |\n|---|---|\n<!-- perf-numbers:end X -->", out)
        self.assertTrue(out.endswith("后记\n"))

    def test_rewrite_is_idempotent(self):
        once, _ = gpn.rewrite(self.DOC, "X", "new content")
        twice, _ = gpn.rewrite(once, "X", "new content")
        self.assertEqual(once, twice, "重复 --write 不应持续改写文件（否则 --check 永远红）")

    def test_missing_marker_reports_zero(self):
        _, hits = gpn.rewrite("no markers here", "X", "x")
        self.assertEqual(hits, 0)

    def test_duplicate_marker_reports_two(self):
        doc = self.DOC + self.DOC
        _, hits = gpn.rewrite(doc, "X", "x")
        self.assertEqual(hits, 2, "重复标记必须能被发现（否则只会改到第一个）")

    def test_changed_block_detected(self):
        """--check 的核心：手改块之后重写结果必然不同。"""
        tampered = self.DOC.replace("old", "手改成 999ms")
        rewritten, _ = gpn.rewrite(tampered, "X", "generated")
        self.assertNotEqual(tampered, rewritten)


class RewriteRowTest(unittest.TestCase):
    """表格行块（整行占位）：表格中间不能塞注释，所以单独一套。"""

    def test_row_replaced_no_blank_lines(self):
        doc = "| a | b |\n|---||---|\nPERF-NUMBERS-ROW:X\n| c | d |\n"
        out, hits = gpn.rewrite_row(doc, "X", "| row | here |")
        self.assertEqual(hits, 1)
        self.assertIn("PERF-NUMBERS-ROW:X\n| c | d |", doc)  # 前提：占位确实被替换
        self.assertIn("\n| row | here |\n| c | d |", out)
        self.assertNotIn("\n\n", out, "表格行之间不能多出空行，否则表格被截断")

    def test_prefix_does_not_shadow_longer_id(self):
        """`site-verdict-row` 是 `site-verdict-row-en` 的前缀：不钉行尾会让英文页渲染出中文行。

        这是真实发生过的回归（英文页出现过一行中文）。
        """
        doc = "PERF-NUMBERS-ROW:site-verdict-row-en\n"
        _, hits = gpn.rewrite_row(doc, "site-verdict-row", "| 中文 |")
        self.assertEqual(hits, 0)
        out, hits = gpn.rewrite_row(doc, "site-verdict-row-en", "| English |")
        self.assertEqual(hits, 1)
        self.assertEqual(out, "| English |\n")

    def test_row_missing_or_duplicate(self):
        self.assertEqual(gpn.rewrite_row("nothing", "X", "y")[1], 0)
        self.assertEqual(gpn.rewrite_row("PERF-NUMBERS-ROW:X\nPERF-NUMBERS-ROW:X\n", "X", "y")[1], 2)


class LatestTest(unittest.TestCase):
    def test_ignores_non_stamped_files(self):
        """bench-T2-4-baseline.json 这类无时间戳存档不能被当成「最新」。"""
        import glob
        import tempfile

        with tempfile.TemporaryDirectory() as d:
            for name in ("bench-20261028-121124.json", "bench-20261117-120235.json", "bench-T2-4-baseline.json"):
                open(os.path.join(d, name), "w").close()
            orig = gpn.RESULTS
            try:
                gpn.RESULTS = d
                got = gpn._latest("bench-*.json")
            finally:
                gpn.RESULTS = orig
            self.assertEqual(os.path.basename(got), "bench-20261117-120235.json")


class RecordTest(unittest.TestCase):
    def test_record_reads_results_key(self):
        """bench-compare.sh 写的是 `results`（旧草案叫 stacks），两个都得认。"""
        import json
        import tempfile

        payload = {
            "generated_at": "2026-11-17T12:02:35+00:00",
            "runs_per_metric": 5,
            "host": {"cpu": "X", "cores": 32, "platform": "Linux-x"},
            "results": [{"stack": "gox", "fib28_ms": 263}, {"stack": "goja", "fib28_ms": 256}],
        }
        with tempfile.TemporaryDirectory() as d:
            path = os.path.join(d, "bench-20261117-120235.json")
            with open(path, "w", encoding="utf-8") as f:
                json.dump(payload, f)
            orig_results, orig_root = gpn.RESULTS, gpn.ROOT
            try:
                gpn.RESULTS = d
                gpn.ROOT = d
                g = gpn.record()
            finally:
                gpn.RESULTS, gpn.ROOT = orig_results, orig_root
        self.assertEqual(g["stacks"]["gox"]["fib28_ms"], 263)
        self.assertEqual(g["source"]["runs"], 5)


if __name__ == "__main__":
    sys.exit(0 if unittest.main(exit=False).result.wasSuccessful() else 1)
