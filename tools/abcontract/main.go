// Command abcontract 是「性能 A/B 契约」闸门（T3-1 / T3-4 第二十三批）。
//
// # 为什么有这个工具
//
// 2026-11-20 夜批的实况：四条候选（dbg 三元组、到站判定、栈头内联、PC 本地副本）
// 都做了同机 8 轮交替 A/B，三条被数字否掉，第四条（PC 本地副本）连 vm 全量测试
// 都过不了 —— 解构迭代与 async generator 直接挂死。这本是正常的负结果，但暴露
// 了一个流程问题：
//
//   - 「有没有做 A/B」在提交里只能靠人写，没有机器判据；
//   - 「实验代码有没有留在树里」同样只能靠人记得删 —— 上一夜留下的孤儿提交
//     （7d6171d / 624042c）就是这么来的；
//   - 「bench-results/*.json 里的数字是否真能回溯到某个 commit」也没人校验。
//
// 于是本仓的纪律（docs/performance.md §3.4.6 / §12.23）在提交那一刻不可验证。
// 这个工具把它变成可验证的：
//
//	abcontract -message <file>   # 校验一条提交信息是否满足 A/B 契约（给 commit-msg 钩子用）
//	abcontract -history <range>  # 校验一段历史里的性能提交都满足契约（给 CI / 手动核查用）
//	abcontract -tree             # 校验工作树里没有残留实验标记（[EXP ...]）
//
// # 判据
//
// 一条**性能提交**（subject 以 perf( / exp( / measure( 开头，或正文含 Bench 数字块）
// 必须满足：
//
//   - 正文里至少一个 A/B 数字：形如 `-1.30%` / `+2.17%` 的符号百分数，或
//     `ns/op` / `allocs/op` 表格行；
//   - 正文里至少一条**可回溯的复现命令**：出现 `perf-ab.sh` / `coldstart.sh` /
//     `go test` / `go tool pprof` 之一；
//   - 若判定为「否 / 撤回 / 不做」，正文必须同时给出**理由**（出现
//     否|撤回|不做|未达阈值 之一）与**数字**（同上）—— 只写"否了"不算。
//
// 「[EXP」开头的实验提交（本机临时验证、不打算合并的那种）**不允许出现在
// main 的历史里**：它们必须被 sign-off 成一条带着数字与结论的正式提交，或干脆
// 不进历史。这条在 -tree 与 -history 两个模式下都查。
//
// # 为什么不直接要求 JSON 结果文件
//
// 结果文件按时间戳命名、每批产生数份，把它们全部入库会让仓库膨胀且无法审阅
// （§3.4.6 只把噪声底与关键 A/B 的 JSON 入库）。所以闸门只校验「提交信息里
// 有数字与命令」，JSON 是否入库由提交者判断 —— 契约管的是**可复核性**，不是
// 存档策略。
//
// 退出码：0 = 合规，1 = 不合规（逐条列出违规），2 = 用法错误。
package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
)

var (
	// percentRe 匹配带符号的百分数（A/B 结果的最小可识别形态）。
	percentRe = regexp.MustCompile(`[+\-−]\s?\d+(\.\d+)?\s?%`)
	// metricRe 匹配基准表格行里的指标名（不依赖百分数也算数字）。
	metricRe = regexp.MustCompile(`(ns/op|allocs/op|B/op|µs|us\b|ms\b)`)
	// reproRe 匹配可复现命令的锚点。
	//
	// 2026-11-20 首批回扫（origin/main~40）暴露了第一版的误报：778392a 正文里
	// 「同一二进制只改预取起跑时间即可复现」本身就是一条**等价于命令**的复现
	// 配方（只改一个变量对照），而第一版只认四种脚本名，于是把这条合格的提交
	// 判成了违规。闸门的误报比漏报更危险 —— 被误判过一次的人会直接绕过它。
	// 所以这里放宽到「任何可执行的复现锚点」：脚本、go 命令、或明确写出的
	// '复现' 段落（含命令式动词）。
	reproRe = regexp.MustCompile(`perf-ab\.sh|coldstart\.sh|go test|go tool pprof|bench-all\.sh|--benchtime|复现命令|复现:\s*\S`)
	// negRe 匹配「否 / 撤回 / 不做」类**判定**。
	//
	// 只认带否定的判定对（否定在前、判定动词在后），不认裸的「撤回」「不做」——
	// 首版就是这么把 21a8cff 误判的：它的「撤回」是在撤回**上一批的错误结论**
	// （结论已被证伪），而不是"这个优化被否了"，正文里的 501ms/251ms/16ms 等
	// 数字本来就在。判定形态收窄到：
	//   - "否" / "否掉" / "不合并" 直接出现；
	//   - "撤回" / "不做" / "未达阈值" 仅在否定语境（前 8 字内有 否/不/已/被/作废/推翻）；
	//   - "这个方向不行 / 不值得做" 一类明确否定句。
	negRe = regexp.MustCompile(`否掉|否了|(^|[，。；\s])否\b|不合并|不做这个|不值得做|未达阈值|(已|被|作废|推翻|判定为?)[^。\n]{0,8}撤回|撤回[^。\n]{0,6}(上一批|结论|误判)`)
	// expRe 匹配实验标记前缀。
	expRe = regexp.MustCompile(`^exp\(|\[EXP `)
)

// isPerfCommit 判定一个 subject 是否属于「性能提交」。
func isPerfCommit(subject string) bool {
	s := strings.TrimSpace(subject)
	for _, p := range []string{"perf(", "exp(", "measure("} {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

// violation 是一条判据失败的原因（带位置，便于直接贴进 CI 注解）。
type violation struct {
	where string
	why   string
}

// checkMessage 校验一条提交信息（subject 为第一行，body 为其余部分）。
func checkMessage(where, subject, body string) []violation {
	var out []violation
	if expRe.MatchString(subject) {
		out = append(out, violation{where, "实验提交（exp( / [EXP）不允许进 main 历史：把结论与 A/B 数字写进一条正式提交，或不要提交"})
	}
	if !isPerfCommit(subject) {
		return out
	}
	if !percentRe.MatchString(body) && !metricRe.MatchString(body) {
		out = append(out, violation{where, "性能提交正文里没有 A/B 数字（需形如 -1.30% 的符号百分数，或 ns/op|allocs/op 表格行）"})
	}
	if !reproRe.MatchString(body) {
		out = append(out, violation{where, "性能提交正文里没有可复现命令（perf-ab.sh / coldstart.sh / go test / go tool pprof 任一）"})
	}
	if negRe.MatchString(body) && !percentRe.MatchString(body) && !metricRe.MatchString(body) {
		out = append(out, violation{where, "写了「否 / 撤回 / 不做」但没附数字：负结果同样要带测量结果，否则下一个人无法判断该不该重做"})
	}
	return out
}

// run 执行 git 命令并返回输出。
func run(args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	out, err := cmd.Output()
	return string(out), err
}

// checkTree 扫描已跟踪文件里残留的实验标记。
//
// 只看「新增行」比看整棵树干净：历史里合法的注释可能偶然含有 "[EXP "。
// 所以这里用 `git grep` 扫工作树，并把命中交给调用者看 —— 工作树里残留实验
// 代码的判定标准是「文件里有 [EXP 标记且它不是已提交内容」。
func checkTree() []violation {
	out, err := run("grep", "-n", "-I", "-e", "[EXP ", "--", "*.go", "*.sh", "*.py")
	if err != nil {
		return nil // git grep 无命中时退出码为 1
	}
	var v []violation
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, ":", 3)
		if len(parts) < 3 {
			continue
		}
		v = append(v, violation{parts[0] + ":" + parts[1], "工作树里残留实验标记 [EXP —— 实验代码必须在提交前删掉（保留结论与数字，不保留实验代码）"})
	}
	return v
}

// checkHistory 逐条校验区间内的提交。
//
// 用 -z + NUL 分隔: 不用 -z 时 git 会给含非 ASCII 的字段加双引号并转义
// (甚至把 %x1f 写成字面量 "^_"), 于是正文永远解析不到 —— 首版就是这么
// 把四条合格提交误判成违规的（自测当时只喂内存字符串，看不出来）。
func checkHistory(rng string) ([]violation, error) {
	out, err := run("log", "-z", "--format=%h%x00%s%x00%b", rng)
	if err != nil {
		return nil, fmt.Errorf("git log %s: %w", rng, err)
	}
	var vs []violation
	for _, rec := range strings.Split(out, "\x00\x00") {
		f := strings.SplitN(strings.TrimLeft(rec, "\x00\n"), "\x00", 3)
		if len(f) < 3 || strings.TrimSpace(f[0]) == "" {
			continue
		}
		vs = append(vs, checkMessage(f[0], f[1], f[2])...)
	}
	return vs, nil
}

func main() {
	var (
		msgFile  = flag.String("message", "", "提交信息文件（给 commit-msg 钩子用）")
		history  = flag.String("history", "", "git 提交区间，如 origin/main..HEAD")
		tree     = flag.Bool("tree", false, "检查工作树里残留的实验标记")
		selfTest = flag.Bool("self-test", false, "自测：判据必须能红也能绿（负向样本必红）")
	)
	flag.Parse()

	if *selfTest {
		os.Exit(selfTestRun())
	}

	var vs []violation
	switch {
	case *msgFile != "":
		data, err := os.ReadFile(*msgFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "abcontract: 读不到提交信息文件 %s: %v\n", *msgFile, err)
			os.Exit(2)
		}
		// git 默认把 # 开头的行当注释（commit-msg 钩子看到的就是这样一份文件），
		// 这里用 --cleanup=verbatim 语义读原文：注释行在下面统一剔除，避免把
		// "请为你的改动写一段说明" 之类的模板当正文。
		lines := strings.SplitN(strings.TrimRight(string(data), "\n"), "\n", 2)
		subject, body := lines[0], ""
		if len(lines) > 1 {
			body = lines[1]
		}
		// 提交信息里的注释行（# 开头）不算正文。
		kept := make([]string, 0, 16)
		for _, l := range strings.Split(body, "\n") {
			if !strings.HasPrefix(l, "#") {
				kept = append(kept, l)
			}
		}
		vs = append(vs, checkMessage("COMMIT_EDITMSG", subject, strings.Join(kept, "\n"))...)
	case *history != "":
		got, err := checkHistory(*history)
		if err != nil {
			fmt.Fprintf(os.Stderr, "abcontract: %v\n", err)
			os.Exit(2)
		}
		vs = append(vs, got...)
	case *tree:
		vs = append(vs, checkTree()...)
	default:
		fmt.Fprintln(os.Stderr, "用法: abcontract -message <file> | -history <range> | -tree | -self-test")
		os.Exit(2)
	}

	if len(vs) == 0 {
		fmt.Println("[abcontract] ✓ A/B 契约满足")
		return
	}
	for _, v := range vs {
		fmt.Fprintf(os.Stderr, "[abcontract] ✗ %s: %s\n", v.where, v.why)
	}
	os.Exit(1)
}

// selfTestRun 是判据的自证：正样本必须绿、负样本必须红。
//
// 一个只会说"通过"的闸门和删掉没区别（与 check-bench.py --self-test、
// tools/initpurity 的 testdata 同一纪律）。
func selfTestRun() int {
	type cas struct {
		name, subject, body string
		wantViolations      int
	}
	cases := []cas{
		{"合格：带数字与命令", "perf(vm): 操作数栈头内联",
			"ns/op 137.9ms → 135.9ms（-1.43%，7/8 轮）\n复现: ./scripts/perf-ab.sh --runs 8", 0},
		{"合格：负结果带数字与理由", "measure(perf): 三条候选被否",
			"候选B +2.17%（0/8）；候选C -9.99%... 判定为否，不合并。\n复现: go test ./tools/perf -bench Fib28", 0},
		{"不合格：性能提交无数字", "perf(vm): 快点", "让解释器更快一点。\n复现: go test ./...", 1},
		{"不合格：无复现命令", "perf(vm): 栈优化", "ns/op -3.20%（8/8）", 1},
		{"不合格：无数字无命令", "perf(vm): 优化", "改动了一些热点。", 2},
		// 两条违规是**有意**的重复归因：小整数缓存那次（-1.66%）正是"否了、
		// 但没写数字"的形态，读的人既不知道该不该重做，也看不到为什么否。
		{"不合格：否了但没数字", "measure(perf): 试了一下", "这个方向否了, 不合并。\n复现: go test ./...", 2},
		{"不合格：实验提交进历史", "exp(perf): [EXP B] 到站判定改比末指令起点",
			"ns/op +2.17%\n复现: go test ./tools/perf", 1},
		{"合格：measure 存档带 µs、复现命令段", "measure(coldstart): 本机首帧基线存档",
			"三段分母: version 2.82ms / startup_idle 3.04ms / SetupGlobals 262µs。\n\n复现命令:\n  ./scripts/coldstart.sh --runs 7 --bin ./gox", 0},
		{"合格：撤回的是上一批结论（不是否优化）", "measure(coldstart): 撤回\"sleep 未生效\"的错误结论",
			"本批把上一批的错误结论撤回: sleep 0.25/0.5/1 → 251/501/1001ms, 伪影窗口 5~10ms。\n复现: ./scripts/check-harness-clock.sh", 0},
		{"不适用：非性能提交", "docs: 补一段说明", "没有数字", 0},
	}
	pass, fail := 0, 0
	for _, c := range cases {
		got := checkMessage("self-test", c.subject, c.body)
		if len(got) == c.wantViolations {
			fmt.Printf("  ✓ %s\n", c.name)
			pass++
		} else {
			fmt.Printf("  ✗ %s（期望 %d 条违规，实际 %d 条）: %+v\n", c.name, c.wantViolations, len(got), got)
			fail++
		}
	}
	fmt.Printf("[abcontract] 自测结果: %d 通过, %d 失败\n", pass, fail)
	if fail > 0 {
		return 1
	}
	return 0
}
