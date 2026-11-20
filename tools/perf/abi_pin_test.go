// 形态钉子: 解释器主循环里"常量下标读局部槽"编译成什么 (T3-4 第二十五批)
//
// 这条闸门是这一轮**唯一的交付物**, 它源自一个被数字否掉的候选:
//
// 候选: Frame 增 `LocalPtrs []unsafe.Pointer` (与 Locals 等长, 每槽存该槽
// interface 的地址), OP_LOAD / OP_STORE 改为 `*(*object.Value)(LocalPtrs[slot])`。
// 理由听上去很硬: 原写法要载入 slice 头 (data/len, 而 cap 那次载入纯属陪跑),
// 指针区写法理论上更短。功能上完全正确 (vm 全量 + CI xvfb 全量全绿), 同机
// 配对 A/B 却是 **+4.30% / +6.37%** (1/24、0/20 轮胜)。三组两两对照拆因:
//
//	只维护指针区、主循环照旧读 Locals  → +4.84%  (2/20)
//	同位置插入一个从未被引用的标记变量 → +0.54% / +4.30% (2/12、1/12)
//
// 于是把两份二进制的 runFrom 反汇编拿出来对, 结论是**两者本来就一样长**:
//
//	Locals 版本 (每槽):                    LocalPtrs 版本 (每槽):
//	  MOVQ 0x10(AX), SI   ; slice len          MOVQ 0x8(AX), SI     ; ptr slice data
//	  CMPQ $0x100, SI     ; 常量上界           LEAQ -0x10(SI)(SI*16), DX
//	  JBE  panic          ; 无下界(int→uint)   MOVQ 0x10(AX), SI    ; ptr slice len
//	  MOVQ 0x8(AX), DX    ; slice data         CMPQ SI, SI          ; len 比较
//	  LEAQ (DX)(SI*16), DX                     CMPQ $0x0, SI        ; 下界
//
// 两边都是 4~5 条, "省一个字段载入"被"多一次 ptr 切片载入 + 多一次比较"吃回去。
// 这说明这条指令的寻址成本是**工具链 ABI 的产物**, 不是源码里能一眼看出的东西。
//
// 因此本测试把当前形态钉住: 任何人再动局部槽寻址, 要么形态不变 (那就不该指望
// 有收益, 用 §12.26 的数字说服自己), 要么形态变了 —— 那时必须重新做同机 A/B,
// 而不是沿用旧结论。
//
// 口径边界 (别把它当性能闸门):
//   - 只判**形态**, 不判快慢。形态变了不等于变慢, 只等于"旧 A/B 的解释失效"。
//   - 不同 GOARCH 生成不同指令序列, 只在 linux/amd64 上硬断言, 其余平台 skip。
//   - 缺 objdump/nm 时 skip (环境问题不判红, 与 coldstart.sh "缺栈跳过"同纪律)。

package perf

import (
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// fixtureFn / fixturePtrFn 是两种同形源码 (正例 = 现在主循环的写法, 反例 =
// 被否掉的指针表写法), 都在 internal/abi_pin_fixture 里。
const (
	fixtureFn    = "github.com/14752222/Gox/internal/abi_pin_fixture.LocalsDirect"
	fixturePtrFn = "github.com/14752222/Gox/internal/abi_pin_fixture.PtrIndirectLoop"
	// fixtureCtlFn 是"同形但只有一层"的对照组: 直接切片 + 可变下标。它也要算
	// 地址, 但不多一层间接 —— 闸门**不能**拦它, 这一对样本界定了判据的敏感度。
	fixtureCtlFn = "github.com/14752222/Gox/internal/abi_pin_fixture.LocalsLoop"
)

// 形态判据 (都是"当前生成物"的事实, 不是规范推导):
//
//   - dataLoadRE    `mov 0x8(%rax),%reg` —— 载入 slice 的 data 字段 (frame 在 %rax,
//     Locals 在 offset 0x8; 常量下标所以**不载入 len**);
//   - slotStoreRE   紧接着出现 `mov %reg,0xNN(%data)` —— 地址折进 store;
//   - slotLoadRE    紧接着出现 `mov 0xNN(%data),%reg` —— 常量下标被折进 disp。
//
// 判据必须**锚在 dataLoad 之后**, 否则 %rsp 栈槽的读写也会命中, 判据就废了
// (第一版就是这么错的: 负样本也被判绿)。
//   - negativeRE    反例的**签名**: 刚载入的 data 从未被用作基址, 而是立刻被
//     "再解引用一次"覆盖掉 (mov (%rax),%rdx; mov 0x8(%rdx),%rdx)。这正是 §12.26
//     被否掉的指针表形态 —— 每槽多一层间接。fixture 里的 PtrIndirect 必须命中它
//     (负样本必红), 而当前的 LocalsDirect 必须**不**命中。
var (
	fixtureInsnRE = regexp.MustCompile(`^\s+[0-9a-f]+:\t(.+)$`)
	dataLoadRE    = regexp.MustCompile(`^mov 0x8\(%rax\),%r[a-z0-9]+$`)
	slotStoreRE   = regexp.MustCompile(`^mov %r[a-z0-9]+,0x[0-9a-f]+\(%r[a-z0-9]+\)$`)
	slotLoadRE    = regexp.MustCompile(`^mov 0x[0-9a-f]+\(%r[a-z0-9]+\),%r[a-z0-9]+$`)
	reloadRE      = regexp.MustCompile(`^mov \(%r[a-z0-9]+\),%r[a-z0-9]+$`)
	indexReloadRE = regexp.MustCompile(`^mov \(%r[a-z0-9]+,%r[a-z0-9]+,8\),%r[a-z0-9]+$`)
)

func TestLocalsAddressingShape(t *testing.T) {
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		t.Skip("形态断言只针对 linux/amd64 (其他架构指令序列不同, 结论不通用)")
	}
	for _, tool := range []string{"objdump", "nm"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("缺 %s, 形态闸门跳过 (环境问题不判红)", tool)
		}
	}
	root := repoRootDir(t)
	bin := t.TempDir() + "/fixture.test"
	build := exec.Command("go", "test", "-c", "-o", bin, "./internal/abi_pin_fixture")
	build.Dir = root
	if b, err := build.CombinedOutput(); err != nil {
		// 编译失败是**真问题** (fixture 被改坏), 不是环境问题。
		t.Fatalf("编译 fixture 失败: %v\n%s", err, b)
	}

	dump, err := exec.Command("objdump", "-d", "--no-show-raw-insn", bin).Output()
	if err != nil {
		t.Fatalf("objdump 失败: %v", err)
	}
	body := extractFunc(string(dump), fixtureFn)
	if body == "" {
		t.Fatalf("反汇编里找不到 %s —— fixture 被改名或被内联掉了, 这条闸门已失效", fixtureFn)
	}
	insns := normalizeInsns(body)
	t.Logf("%s 当前形态 (%d 条):\n%s", fixtureFn, len(insns), strings.Join(insns, "\n"))

	// 判据零: 不得出现指针表的二次间接签名。
	if hasIndirectionSignature(insns) {
		t.Fatalf("局部槽寻址出现了指针表的二次间接形态 —— 这正是本轮被同机 A/B 否掉的做法\n"+
			" (+4.30%%, 1/24 轮胜, §12.26)。\n当前指令:\n%s", strings.Join(insns, "\n"))
	}

	// 判据一: 载入 data 之后出现直接寻址的写槽。
	if !haveDataLoadThen(insns, slotStoreRE) {
		t.Fatalf("局部槽写入不再走直接寻址 (mov %%reg,0xNN(%%base)) —— "+
			"说明寻址被拆成额外汇编步骤。\n当前指令:\n%s\n"+
			"处置: 若这是有意的改动, 必须按 docs/performance.md §12.26 做同机配对 A/B"+
			"(scripts/perf-ab.sh, ≥16 轮, 并附 --aa 对照), 不许沿用本轮 '指针区更短' 的旧结论。",
			strings.Join(insns, "\n"))
	}

	// 判据二: 载入 data 之后出现单条带立即数偏移的读槽, 且读出的就是槽的值
	// (双寄存器: interface 的 type+data 一起取出), 不是"先取地址再解引用"。
	if !haveDataLoadThen(insns, slotLoadRE) {
		t.Fatalf("读槽不再是单条带立即数偏移的载入 —— 生成物变了"+
			"(指针表/二次间接都会这样, 见 §12.26)。\n当前指令:\n%s",
			strings.Join(insns, "\n"))
	}

	// 判据三: 写入侧仍走写屏障 (interface 槽赋值不可绕过)。
	if !strings.Contains(body, "writeBarrier") && !strings.Contains(body, "gcWriteBarrier") {
		t.Fatalf("局部槽写入不再经过写屏障 —— 这是 GC 正确性问题, 不是性能问题。\n%s", body)
	}

	// 判据四 (负样本必红): 反例样本必须命中判据零。没有这一条, 本闸门只是
	// "打印一下形态", 拦不住任何人 —— 第一版就是这样: 正则连 %rsp 栈槽都匹配,
	// 负样本被判绿, 等于没有闸门。
	ptrInsns := normalizeInsns(extractFunc(string(dump), fixturePtrFn))
	if len(ptrInsns) == 0 {
		t.Fatalf("反汇编里找不到反例样本 %s —— 形态闸门的负样本丢了, 它挡不住任何东西", fixturePtrFn)
	}
	if !hasIndirectionSignature(ptrInsns) {
		t.Fatalf("反例样本 %s 不再呈现二次间接签名 —— 判据零已失效, 它挡不住任何东西:\n%s",
			fixturePtrFn, strings.Join(ptrInsns, "\n"))
	}
	// 判据四b (敏感度): 同形对照组必须**不**命中 —— 否则判据等于"凡算地址就红",
	// 那它拦的是所有可变下标访问, 而不只是指针表。
	ctlInsns := normalizeInsns(extractFunc(string(dump), fixtureCtlFn))
	if len(ctlInsns) == 0 {
		t.Fatalf("反汇编里找不到对照组样本 %s", fixtureCtlFn)
	}
	if hasIndirectionSignature(ctlInsns) {
		t.Fatalf("对照组 %s 也命中间接签名 —— 判据不区分『多一层间接』与『算地址』, "+
			"请收紧签名识别:\n%s", fixtureCtlFn, strings.Join(ctlInsns, "\n"))
	}
}

// hasIndirectionSignature 判断局部槽访问是否多出一层间接 —— 指针表 (LocalPtrs
// 一类) 的签名。两种下标形态各有一条:
//
//   - 常量下标: data 载入后寄存器立刻被当作指针再解引用 (`mov (%rdx),%rdx`);
//   - 可变下标: `mov (%rdx,%rbx,8),%rdx` —— 切片元素本身就是地址。
//
// 直接切片 + 可变下标的对照组是 `mov (%rdx),reg; lea (reg)(idx,16),reg`:
// 同样算地址, 但元素是值不是地址 —— 所以闸门不拦它。
func hasIndirectionSignature(insns []string) bool {
	return hasValueIndirection(insns) || hasAddressIndirection(insns)
}

// hasValueIndirection: 常量下标 + 指针表 (或"切片元素即地址")。
func hasValueIndirection(insns []string) bool {
	if anyMatch(insns, indexReloadRE) {
		return true
	}
	for i, ins := range insns {
		if dataLoadRE.FindStringSubmatch(ins) == nil {
			continue
		}
		reg := strings.SplitN(ins, ",", 2)[1]
		for j := i + 1; j < len(insns) && j <= i+2; j++ {
			r := reloadRE.FindStringSubmatch(insns[j])
			if r != nil && r[1] == reg {
				return true
			}
		}
	}
	return false
}

// hasAddressIndirection: 可变下标 + 指针表 —— `mov (data),(data)` 把切片元素
// (一个地址) 取出来再作为基址使用。切片形态的对照组是
// `mov (data),reg; lea (reg)(idx,16),reg`, 两者只差"谁当基址", 所以这里只认
// "元素即基址"这一种。
func hasAddressIndirection(insns []string) bool {
	for i, ins := range insns {
		if i == 0 || !reloadRE.MatchString(ins) {
			continue
		}
		// data 载入允许落在紧邻的前 2 条内 (中间可能夹一条边界检查后的跳转)。
		for k := i - 1; k >= 0 && k >= i-2; k-- {
			if dataLoadRE.FindStringSubmatch(insns[k]) != nil {
				return true
			}
		}
	}
	return false
}

// haveDataLoadThen 判断序列里"载入 Locals.data 之后的若干条内"是否出现 pattern。
// 4 条窗口足够盖住 LEAQ/写屏障前的地址计算, 又不会串到别的槽。
func haveDataLoadThen(insns []string, pattern *regexp.Regexp) bool {
	for i, ins := range insns {
		if !dataLoadRE.MatchString(ins) {
			continue
		}
		for j := i + 1; j < len(insns) && j <= i+4; j++ {
			if pattern.MatchString(insns[j]) {
				return true
			}
		}
	}
	return false
}

// anyMatch 判断指令序列里是否有一条满足 pattern。
func anyMatch(insns []string, pattern *regexp.Regexp) bool {
	for _, ins := range insns {
		if pattern.MatchString(ins) {
			return true
		}
	}
	return false
}

// extractFunc 从 objdump 文本里截出指定函数体。
func extractFunc(dump, fn string) string {
	lines := strings.Split(dump, "\n")
	start := -1
	for i, l := range lines {
		if strings.HasSuffix(strings.TrimSpace(l), "<"+fn+">:") {
			start = i
			break
		}
	}
	if start < 0 {
		return ""
	}
	var b strings.Builder
	for _, l := range lines[start+1:] {
		if isFuncHeader(l) {
			break
		}
		b.WriteString(l)
		b.WriteString("\n")
	}
	return b.String()
}

// isFuncHeader 判断一行是否是下一个函数的起始行 ("0000000000000000 <pkg.Fn>:")。
func isFuncHeader(line string) bool {
	t := strings.TrimSpace(line)
	if !strings.HasSuffix(t, ">:") || !strings.Contains(t, " <") {
		return false
	}
	head := t[:strings.Index(t, " ")]
	if len(head) < 8 {
		return false
	}
	for _, c := range head {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return false
		}
	}
	return true
}

// normalizeInsns 只保留指令文本 (去掉地址、目标注解), 输出可读、可比对。
func normalizeInsns(body string) []string {
	var out []string
	for _, line := range strings.Split(body, "\n") {
		m := fixtureInsnRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		ins := m[1]
		if idx := strings.Index(ins, " # "); idx > 0 {
			ins = ins[:idx]
		}
		out = append(out, strings.Join(strings.Fields(ins), " "))
	}
	return out
}

// repoRootDir 从 go env GOMOD 推仓库根。
func repoRootDir(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("go", "env", "GOMOD").Output()
	mod := strings.TrimSpace(string(out))
	if err != nil || mod == "" || mod == os.DevNull {
		t.Skip("拿不到 GOMOD, 形态闸门跳过")
	}
	return strings.TrimSuffix(mod, string(os.PathSeparator)+"go.mod")
}
