package gfx

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"testing"
	"time"

	"golang.org/x/image/font"
)

// 冷启动线 (T3-5) 的**分阶段**探针: 把"首次用字体"拆成两段, 分别计时。
//
// 为什么要分两段: 进程级冷启动 (scripts/coldstart.sh) 是一个黑盒数字, 它降了
// 10% 你也不知道是扫描便宜了还是别的地方便宜了。真实首帧路径只有两步:
//
//  1. 目录扫描 (scanSystemFonts): 遍历字体目录, 对每个字体文件试解析。本机
//     Linux 上是 330 个文件 / 148MB —— 冷启动里最贵的一步, 且是**纯 IO**。
//  2. 选默认字体 (chooseBaseFont): 挑首个正体面。它曾经把扫描过的首个候选
//     再从磁盘重读一遍 (19MB, ~15ms)。
//
// 两段都必须是"冷"的: 扫描自带 sync.Once, 缓存是进程级的, 所以每个用例
// 先 resetFontCaches 再计时。这样即便同一个进程里别的用例已经烤热了页缓存
// (那部分无法消除, 也不该消除 —— 真实用户第二次启动就是热的), 测到的仍是
// "引擎自己有没有重复劳动"。
//
// 跑法 (也是闸门的数据来源, 见 scripts/check-coldstart.py):
//
//	go test ./gfx/ -run TestColdstartFontStage -v
//
// 产物: bench-results/coldstart-font-<UTC时间>.json (gitignored, 与
// coldstart-*.json 同级纪律)。

// fontStageRun 是一次分阶段测量的结果 (单位: 毫秒)。
type fontStageRun struct {
	ScanMs   float64 `json:"scan_ms"`
	ChooseMs float64 `json:"choose_ms"`
	Files    int     `json:"scanned_files"`
}

// resetFontStageCaches 把字体子系统恢复到"本次启动刚开始"的状态。
func resetFontStageCaches() {
	fontMu.Lock()
	baseFont = nil
	faceBySize = map[int]font.Face{}
	fontMu.Unlock()
	scannedFonts = nil
	fontCandsOnce = sync.Once{}
	rebuildCandidatesLocked()
	resetFontCaches()
}

// TestColdstartFontStage 测量"扫描"与"选默认字体"两段, 落盘 JSON。
//
// 只在非 Windows 上跑: Windows 的 staticCandidate 列表没配扫描目录
// (fontScanDirs 返回 nil, 见其注释), 扫描段恒为 0, 测了也是噪声。
func TestColdstartFontStage(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 不扫字体目录 (fontScanDirs 返回 nil), 本探针无意义")
	}
	reps := 5
	if v := os.Getenv("PERF_COLDSTART_REPS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			reps = n
		}
	}

	measure := func() fontStageRun {
		resetFontStageCaches()
		t0 := time.Now()
		initFontCandidates()
		tScan := time.Now()
		if _, err := chooseBaseFont(fontCandidates); err != nil {
			t.Fatalf("chooseBaseFont: %v", err)
		}
		tChoose := time.Now()
		return fontStageRun{
			ScanMs:   float64(tScan.Sub(t0)) / float64(time.Millisecond),
			ChooseMs: float64(tChoose.Sub(tScan)) / float64(time.Millisecond),
			Files:    len(scannedFonts),
		}
	}

	measure() // 预热: 首次的目录项缓存 / 二进制页缓存不计入
	runs := make([]fontStageRun, 0, reps)
	for i := 0; i < reps; i++ {
		r := measure()
		t.Logf("[第 %d/%d 次] 扫描=%.2fms 选默认=%.2fms 文件=%d", i+1, reps, r.ScanMs, r.ChooseMs, r.Files)
		runs = append(runs, r)
	}
	scanMed := medianOf(runs, func(r fontStageRun) float64 { return r.ScanMs })
	chooseMed := medianOf(runs, func(r fontStageRun) float64 { return r.ChooseMs })
	t.Logf("字体冷启动分阶段 (中位, n=%d): 扫描=%.2fms 选默认=%.2fms 合计=%.2fms 文件=%d",
		reps, scanMed, chooseMed, scanMed+chooseMed, runs[0].Files)

	dir := filepath.Join("..", "bench-results")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("创建 bench-results: %v", err)
	}
	path := filepath.Join(dir, "coldstart-font-"+time.Now().UTC().Format("20060102-150405")+".json")
	if err := writeColdstartJSON(path, map[string]any{
		"generated_at": time.Now().UTC().Format(time.RFC3339),
		"benchmark":    "gfx/TestColdstartFontStage",
		"command":      "go test ./gfx/ -run TestColdstartFontStage -v",
		"runs":         reps,
		"phases_ms": map[string]float64{
			"scan_ms":   scanMed,
			"choose_ms": chooseMed,
			"total_ms":  scanMed + chooseMed,
		},
		"scanned_files": runs[0].Files,
		"all_runs":      runs,
		"host": map[string]any{
			"platform": runtime.GOOS + "/" + runtime.GOARCH,
			"cores":    runtime.NumCPU(),
			"go":       runtime.Version(),
		},
		"metric_defs": map[string]string{
			"phases_ms.scan_ms":   "scanSystemFonts: 遍历 fontScanDirs() 并逐个试解析字体文件 (每文件只解正体面, 文件级缓存已重置)",
			"phases_ms.choose_ms": "chooseBaseFont: 在候选表里挑首个正体面 (扫描结果在前; 命中文件级缓存, 只 stat)",
			"scanned_files":       "扫描判定为可解析的字体文件数",
		},
		"note": "进程内分阶段口径, 首次用字体时发生; 与进程级 (scripts/coldstart.sh) 和进程内首帧 (tools/perf) 均不可混用",
	}); err != nil {
		t.Fatalf("写 %s: %v", path, err)
	}
	t.Logf("结果已存档: %s", path)
}

func medianOf(runs []fontStageRun, f func(fontStageRun) float64) float64 {
	xs := make([]float64, len(runs))
	for i, r := range runs {
		xs[i] = f(r)
	}
	for i := 1; i < len(xs); i++ {
		for j := i; j > 0 && xs[j] < xs[j-1]; j-- {
			xs[j], xs[j-1] = xs[j-1], xs[j]
		}
	}
	n := len(xs)
	if n%2 == 1 {
		return xs[n/2]
	}
	return (xs[n/2-1] + xs[n/2]) / 2
}

func writeColdstartJSON(path string, v any) error {
	buf, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, buf, 0o644)
}
