package gfx

import (
	"bytes"
	"regexp"
	"strings"
	"testing"
)

// ===== 首帧阶段探针的闸门 (T3-5 第二十六批) =====
//
// 这个探针的价值全在"能不能被人信任"上, 所以钉两条:
//  1. 关着的时候**一个字都不许输出** —— 它是默认路径, 任何"顺手打一行"
//     都会污染所有 GUI 脚本的 stderr (用户拿 grep 抓自己的日志时会撞上);
//  2. 开着的时候格式稳定: 前缀 `gox-phase`、相对时刻带符号与 ms、名字原样。
//     下一批要拿它做 A/B 解析, 格式漂移会让脚本静默读不到数。
func TestStartupPhaseSilentWhenDisabled(t *testing.T) {
	var buf bytes.Buffer
	old := phaseWriter
	phaseWriter = &buf
	defer func() { phaseWriter = old }()

	if StartupPhaseActive() {
		t.Fatal("默认必须是关闭的: 库不能因为被链接就写 stderr")
	}
	PhaseTrace("mount.enter")
	if buf.Len() != 0 {
		t.Fatalf("关着时输出了 %q", buf.String())
	}
}

func TestStartupPhaseFormat(t *testing.T) {
	var buf bytes.Buffer
	old := phaseWriter
	phaseWriter = &buf
	EnableStartupPhase()
	defer func() {
		phaseWriter = old
		startupPhaseOn.Store(false) // 复位, 免得污染同包其它测试
	}()

	PhaseTrace("frame.layout_done")
	got := strings.TrimSpace(buf.String())
	// 形如: gox-phase    +1.244ms frame.layout_done
	re := regexp.MustCompile(`^gox-phase\s+[+-]\d+\.\d{3}ms frame\.layout_done$`)
	if !re.MatchString(got) {
		t.Fatalf("格式漂移: %q", got)
	}
}
