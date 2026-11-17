package object

// regexp2 降级引擎（T2 根因单 rvDg77，2026-11-17 夜批）。
//
// ## 为什么要降级引擎
//
// Go 标准库 regexp 是 RE2：**没有回溯**，因此永远实现不了三类 JS 正则特性：
//   1. 后向引用 \1..\9 与 \k<name>；
//   2. lookbehind 断言 (?<=…) / (?<!…)；
//   3. 组内作用域修饰符的"移除"形态 (?i-:body)（RE2 只支持 (?i:body) 加标志）。
// T2 的失败面（RegExp/named-groups、lookBehind、regexp-modifiers 三族及散布
// 在 String/prototype 等处的同名用法，全仓约 196 条）基本全部落在 RE2 编译
// 失败上 —— 不是翻译写错，是 RE2 的原理性上限。
//
// ## 设计：混合引擎，RE2 路径一行不改
//
// NewRegExp 先走既有 RE2 路径（jsToGoRegex → regexp.Compile）。**只有 RE2 编译
// 失败时**才用 regexp2（dlclark/regexp2，.NET 风格，支持回溯/lookbehind/修饰符
// 组/重复命名组）重试一次。这样：
//   - 所有现在能过的用例走的代码路径与改动前完全一致 ⇒ 全量 A/B 零退潮是
//     结构保证，不是"测出来没坏"；
//   - 只有原本就 compile 失败（= 用例必然失败）的模式才落到新引擎，任何新
//     行为都是纯增量。
//
// ## 索引单位差异（最容易踩的坑）
//
// regexp2 的 Match/Group/Capture 的 Index/Length 按 **rune** 计（实测
// FindStringMatch("ααβab") 对 "ab" 报 Index=3，byte 偏移应为 6）。而本引擎
// 所有消费方沿用 Go regexp 的 **byte 偏移**协议。包装层统一做 rune→byte
// 换算（byteOffsetAt / runeIdxOf），不散落。
//
// ## 已知局限（如实记录，不假装全覆盖）
//
//   - \u{HexDigits} 在 .NET 语法里不存在，降级前转 \uXXXX / 代理对字面；
//   - 属性转义 \p{Name=Value} 长名形态 .NET 不全认 —— 但落进降级引擎的
//     模式多以回溯/lookbehind 为致命差异，属性转义场景 RE2 侧已有
//     translatePropertyEscapes 处理；
//   - 'y'（sticky）/ 'd'（hasIndices）不进模式文本：前者在 lastIndex 层实现，
//     后者在结果包装层实现，降级引擎无需关心。

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/dlclark/regexp2"
)

// decodeRuneAt 返回 off 处的 rune 与其 byte 长度。
func decodeRuneAt(s string, off int) (rune, int) {
	return utf8.DecodeRuneInString(s[off:])
}

// compileFallback 用 regexp2 编译原始 JS 模式（标志走 regexp2 选项而非模式
// 前缀，保持 JS 语义与 RE2 侧组装方式的对应）。失败返回 err，调用方折算成
// 统一的 SyntaxError 文案，不向外暴露 .NET 风格报错。
func compileFallback(pattern string, r *RegExp) (*regexp2.Regexp, error) {
	opts := regexp2.None
	if r.IgnoreCase {
		opts |= regexp2.IgnoreCase
	}
	if r.Multiline {
		opts |= regexp2.Multiline
	}
	if r.DotAll {
		opts |= regexp2.Singleline
	}
	return regexp2.Compile(dotNetPattern(pattern, r.Unicode), opts)
}

// dotNetPattern 把 JS 模式里 .NET 不认的转义调成等价形式：
//   - \u{HexDigits}（u 标志）→ \uXXXX（BMP）或代理对转义（astral）；
//   - \u{...} 不带 u 标志 → 字面 u（Annex B IdentityEscape，与 RE2 侧
//     translateUnicodeEscapes 同口径）；
//   - \uXXXX .NET 原生支持，原样保留。
func dotNetPattern(pattern string, unicode bool) string {
	if !strings.Contains(pattern, `\u`) {
		return pattern
	}
	var b strings.Builder
	b.Grow(len(pattern))
	for i := 0; i < len(pattern); i++ {
		c := pattern[i]
		if c != '\\' || i+1 >= len(pattern) || pattern[i+1] != 'u' {
			b.WriteByte(c)
			continue
		}
		if i+2 < len(pattern) && pattern[i+2] == '{' {
			end := strings.IndexByte(pattern[i+3:], '}')
			if end >= 0 {
				hex := pattern[i+3 : i+3+end]
				if cp, ok := parseHexRun(hex); ok && int(cp) <= 0x10FFFF {
					if unicode {
						writeCodePointAsDotNet(&b, int(cp))
						i = i + 3 + end // '}'；循环 i++ 后越过
						continue
					}
					// 非 u 标志：IdentityEscape → 字面 u
					b.WriteByte('u')
					i = i + 3 + end
					continue
				}
			}
			// 畸形 \u{...}：原样保留，让 .NET 报错（JS 侧同 SyntaxError）
			b.WriteString(`\u`)
			continue
		}
		b.WriteString(`\u`)
		i++
	}
	return b.String()
}

// writeCodePointAsDotNet 把码点写成 .NET 转义：BMP 用 \uXXXX，astral 写
// 代理对转义（类外等价于匹配该码点；astral 区间在类内不展开 —— 与 RE2
// 侧已知局限一致）。
func writeCodePointAsDotNet(b *strings.Builder, cp int) {
	const hexDigits = "0123456789abcdef"
	writeHex4 := func(v int) {
		b.WriteString(`\u`)
		b.WriteByte(hexDigits[(v>>12)&0xF])
		b.WriteByte(hexDigits[(v>>8)&0xF])
		b.WriteByte(hexDigits[(v>>4)&0xF])
		b.WriteByte(hexDigits[v&0xF])
	}
	if cp <= 0xFFFF {
		writeHex4(cp)
		return
	}
	v := cp - 0x10000
	writeHex4(0xD800 + (v >> 10))
	writeHex4(0xDC00 + (v & 0x3FF))
}

// byteOffsetAt 返回 s 的第 runeIdx 个 rune 的 byte 偏移；runeIdx <= 0 → 0，
// 超界 → len(s)。
func byteOffsetAt(s string, runeIdx int) int {
	if runeIdx <= 0 {
		return 0
	}
	n := 0
	for i := range s {
		if n == runeIdx {
			return i
		}
		n++
	}
	return len(s)
}

// runeIdxOf 返回 byte 偏移 byteOff 所在（或其后继）rune 的序号。空匹配前进
// 逻辑只依赖"单调不减"，边界外返回 runeCount。
func runeIdxOf(s string, byteOff int) int {
	n := 0
	for i := range s {
		if i >= byteOff {
			return n
		}
		n++
	}
	return n
}

// matchLocs 把 regexp2 的 Match 转成 Go regexp 风格 byte 偏移 loc 数组
// （2*k 元素；未参与的组 -1,-1）。组 0 = 整匹配。语义对齐 Go Submatch：
// 每组取最后一次捕获。
func matchLocs(s string, m *regexp2.Match) []int {
	groups := m.Groups()
	loc := make([]int, 0, len(groups)*2)
	for _, g := range groups {
		if len(g.Captures) == 0 {
			loc = append(loc, -1, -1)
			continue
		}
		cap := g.Captures[len(g.Captures)-1]
		start := byteOffsetAt(s, cap.Index)
		end := byteOffsetAt(s, cap.Index+cap.Length)
		loc = append(loc, start, end)
	}
	return loc
}

// ── 以下是与 *regexp.Regexp 同名同协议的包装方法 ─────────────────────────
// 消费方（stdlib/regexp.go、stdlib/string_methods.go）一律改调这些方法，
// 不再直接摸 re.Regexp：RE2 路径纯转发，降级路径做 rune→byte 换算。

// hasFallbackEngine 是否已落到降级引擎。
func (r *RegExp) hasFallbackEngine() bool { return r.Regexp2 != nil }

func (r *RegExp) MatchString(s string) bool {
	if !r.hasFallbackEngine() {
		return r.Regexp.MatchString(s)
	}
	ok, _ := r.Regexp2.MatchString(s)
	return ok
}

func (r *RegExp) FindStringIndex(s string) []int {
	if !r.hasFallbackEngine() {
		return r.Regexp.FindStringIndex(s)
	}
	locs := r.fallbackFindAllFrom(s, 0, 1)
	if len(locs) == 0 {
		return nil
	}
	return locs[0]
}

func (r *RegExp) FindStringSubmatchIndex(s string) []int {
	if !r.hasFallbackEngine() {
		return r.Regexp.FindStringSubmatchIndex(s)
	}
	locs := r.fallbackFindAllFrom(s, 0, 1)
	if len(locs) == 0 {
		return nil
	}
	return locs[0]
}

func (r *RegExp) FindAllStringSubmatchIndex(s string, n int) [][]int {
	if !r.hasFallbackEngine() {
		return r.Regexp.FindAllStringSubmatchIndex(s, n)
	}
	return r.fallbackFindAllFrom(s, 0, n)
}

func (r *RegExp) FindAllString(s string, n int) []string {
	if !r.hasFallbackEngine() {
		return r.Regexp.FindAllString(s, n)
	}
	locs := r.fallbackFindAllFrom(s, 0, n)
	if locs == nil {
		return nil
	}
	out := make([]string, 0, len(locs))
	for _, loc := range locs {
		out = append(out, s[loc[0]:loc[1]])
	}
	return out
}

// SubexpNames 组名数组：下标 0 恒为 ""（整匹配，对齐 Go 口径）；未命名组
// 也是 ""。regexp2 的 GetGroupNames 会把未命名组报成序号字符串（"1"），
// 在此归一。
func (r *RegExp) SubexpNames() []string {
	if !r.hasFallbackEngine() {
		return r.Regexp.SubexpNames()
	}
	names := r.Regexp2.GetGroupNames()
	out := make([]string, len(names))
	out[0] = ""
	for i := 1; i < len(names); i++ {
		if names[i] == strconv.Itoa(i) {
			out[i] = ""
		} else {
			out[i] = names[i]
		}
	}
	return out
}

// Split 对齐 regexp.Split 的 limit 语义：n<0 全部；n>0 最多 n 个子串
// （最后一个含余部）；n==0 返回 nil。
func (r *RegExp) Split(s string, n int) []string {
	if !r.hasFallbackEngine() {
		return r.Regexp.Split(s, n)
	}
	if n == 0 {
		return nil
	}
	locs := r.fallbackFindAllFrom(s, 0, n-1)
	out := []string{}
	prev := 0
	for _, loc := range locs {
		out = append(out, s[prev:loc[0]])
		prev = loc[1]
	}
	out = append(out, s[prev:])
	return out
}

// ReplaceAllStringFunc 对齐 regexp.ReplaceAllStringFunc：逐匹配替换，空匹配
// 后前进一个 rune，避免原地死循环（Go 同款处理）。
func (r *RegExp) ReplaceAllStringFunc(s string, f func(string) string) string {
	if !r.hasFallbackEngine() {
		return r.Regexp.ReplaceAllStringFunc(s, f)
	}
	var b strings.Builder
	prev := 0
	for _, loc := range r.fallbackFindAllFrom(s, 0, -1) {
		b.WriteString(s[prev:loc[0]])
		b.WriteString(f(s[loc[0]:loc[1]]))
		prev = loc[1]
		if loc[0] == loc[1] {
			// 空匹配：把当前 rune 原样带过（Go regexp 的 ReplaceAll 对空
			// 匹配后进一格的语义，函数式接口里表现为跳过下一个重叠匹配）
			b.WriteString(s[loc[1] : loc[1]+runeLenAt(s, loc[1])])
			prev = loc[1] + runeLenAt(s, loc[1])
		}
	}
	b.WriteString(s[prev:])
	return b.String()
}

func runeLenAt(s string, off int) int {
	if off >= len(s) {
		return 0
	}
	_, size := decodeRuneAt(s, off)
	return size
}

// ── 降级引擎的匹配推进 ────────────────────────────────────────────────────

// fallbackFindFrom 从 rune 起点 startRune 开始找首个匹配。
func (r *RegExp) fallbackFindFrom(s string, startRune int) *regexp2.Match {
	if startRune <= 0 {
		m, err := r.Regexp2.FindStringMatchStartingAt(s, 0)
		if err != nil || m == nil {
			return nil
		}
		return m
	}
	// regexp2 无"从偏移查找首个匹配且不与 locs 混用"的便捷口 —— 从起点起
	// 收 1 个即可（空匹配前进逻辑保证不重不漏）。
	locs := r.fallbackFindAllFrom(s, startRune, 1)
	if len(locs) == 0 {
		return nil
	}
	// 重建 Match 等价信息对 FindStringIndex 无必要 —— 它只用 loc[0]/loc[1]。
	// 这里仅 FindStringSubmatchIndex/FindStringIndex 进入, 两者都走
	// fallbackFindAllFrom。此函数保留给单匹配入口, 直接返回 nil 走空结果。
	return nil
}

// fallbackFindAllFrom 从 rune 起点 startRune 收集至多 n 个匹配（n<0 全部）。
// 空匹配后前进一个 rune，对齐 RE2 语义。返回 byte 偏移 loc 数组。
func (r *RegExp) fallbackFindAllFrom(s string, startRune int, n int) [][]int {
	out := [][]int{}
	pos := startRune // rune 游标
	for n < 0 || len(out) < n {
		if pos > runeCount(s) {
			break
		}
		m, err := r.Regexp2.FindStringMatchStartingAt(s, pos)
		if err != nil || m == nil {
			break
		}
		loc := matchLocs(s, m)
		out = append(out, loc)
		endRune := runeIdxOf(s, loc[1])
		if endRune <= pos {
			endRune = pos + 1 // 空匹配：前进一个 rune
		}
		pos = endRune
	}
	if len(out) == 0 && n > 0 {
		return nil
	}
	return out
}

func runeCount(s string) int {
	n := 0
	for range s {
		n++
	}
	return n
}

// ── 命名组名预校验（T2 补充, 2026-11-17）────────────────────────────────
//
// 规范 (prod-GroupSpecifier): 组名首字符 ∈ ID_Start ∪ {$,_}, 其余 ∈
// ID_Continue ∪ {$,_} (ID_Continue 含 ZWNJ/ZWJ \u200C\u200D)。且组名里的
// \uXXXX / \u{…} 转义先解码成字符再校验 —— 这是独立于引擎选择的早错误:
// RE2 会放行 `(?<$>` 这类非法名, .NET 又拒绝名字里的 \uXXXX 转义形态, 所以
// 必须在两条引擎路径之前统一处理: 非法名 → SyntaxError; 合法名里的转义 →
// 解码成字面字符 (astral 写成代理对字面, 与 dotNetPattern 同口径)。

// preprocessGroupNames 扫描 pattern 里的 (?<name>, 校验并解码组名。
// 返回改写后的 pattern；非法名返回错误（NewRegExp 折算 SyntaxError）。
func preprocessGroupNames(pattern string) (string, error) {
	if !strings.Contains(pattern, "(?<") {
		return pattern, nil
	}
	var b strings.Builder
	b.Grow(len(pattern))
	for i := 0; i < len(pattern); {
		if !strings.HasPrefix(pattern[i:], "(?<") {
			b.WriteByte(pattern[i])
			i++
			continue
		}
		j := i + 3
		if j < len(pattern) && (pattern[j] == '=' || pattern[j] == '!') {
			// lookbehind: 不碰
			b.WriteString("(?<")
			i = j
			continue
		}
		// 找组名结束的 '>' (考虑 \> 转义不太可能, 规范里组名不含裸 >)
		end := -1
		for k := j; k < len(pattern); k++ {
			if pattern[k] == '\\' {
				k++ // 跳过转义字符
				continue
			}
			if pattern[k] == '>' {
				end = k
				break
			}
		}
		if end < 0 {
			// 名字没闭合: 交给引擎报错 (JS 同为 SyntaxError)
			b.WriteString(pattern[i:])
			break
		}
		name := pattern[j:end]
		decoded, ok := decodeGroupName(name)
		if !ok {
			return "", fmt.Errorf("Invalid capture group name")
		}
		b.WriteString("(?<")
		b.WriteString(decoded)
		b.WriteString(">")
		i = end + 1
	}
	return b.String(), nil
}

// decodeGroupName 解码组名里的 \uXXXX / \u{…} 转义并校验字符集。
func decodeGroupName(name string) (string, bool) {
	var b strings.Builder
	runes := []rune{}
	for i := 0; i < len(name); {
		c := name[i]
		if c != '\\' {
			r, size := utf8.DecodeRuneInString(name[i:])
			b.WriteRune(r)
			runes = append(runes, r)
			i += size
			continue
		}
		if i+1 >= len(name) {
			return "", false
		}
		switch name[i+1] {
		case 'u':
			if i+2 < len(name) && name[i+2] == '{' {
				end := strings.IndexByte(name[i+3:], '}')
				if end < 0 {
					return "", false
				}
				hex := name[i+3 : i+3+end]
				v, ok := parseHexRun(hex)
				if !ok || v > 0x10FFFF {
					return "", false
				}
				r := rune(v)
				b.WriteRune(r)
				runes = append(runes, r)
				i = i + 3 + end + 1
				continue
			}
			if i+6 <= len(name) {
				v, ok := parseHexRun(name[i+2 : i+6])
				if !ok {
					return "", false
				}
				r := rune(v)
				b.WriteRune(r)
				runes = append(runes, r)
				i += 6
				continue
			}
			return "", false
		default:
			// 其他转义 (如 \r) 规范不允许出现在组名里 → SyntaxError
			return "", false
		}
	}
	// 字符集校验: 首字符 ID_Start ∪ {$,_}; 其余 ID_Continue ∪ {$,_}
	// ID_Start ≈ Letter; ID_Continue ≈ Letter|Digit|Mark; ZWNJ/ZWJ 显式收录。
	isStart := func(r rune) bool {
		if r == '$' || r == '_' {
			return true
		}
		return unicode.IsLetter(r)
	}
	isCont := func(r rune) bool {
		if r == '$' || r == '_' || r == 0x200C || r == 0x200D {
			return true
		}
		return unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.IsMark(r)
	}
	if len(runes) == 0 || !isStart(runes[0]) {
		return "", false
	}
	for _, r := range runes[1:] {
		if !isCont(r) {
			return "", false
		}
	}
	return b.String(), true
}

// hasFallbackTrigger 降级引擎准入闸门（零退潮的结构保证之二）。
//
// 负向测试（test262 期望 SyntaxError 的用例）以前靠 RE2 编译失败"捡到"错误；
// 降级引擎更宽容，会把其中一部分放行成合法。所以降级不是无条件兜底，而是
// 只对「RE2 原理性做不了、且 JS 语法本身合法」的特征开放：
//
//   - 回溯引用 \1..\9 / \k<name>；
//   - lookbehind (?<= / (?<!；
//   - 组内作用域修饰符 (?ims-ims:…)（成员集合法性已由 checkModifiersGroup
//     在之前统一校验，'d'/'v'/'y'/空集进不来）。
//
// 附加限制：
//   - v 标志（unicodeSets）永不降级 —— v 语法完整物化成 RE2 类集合，RE2
//     编译失败即 JS 语法非法（unsupported property / 嵌套类错误等负向用例
//     全在这里）；
//   - u 标志只对回溯/lookbehind/\k 开放 —— 属性转义 \p{…} 的"不支持属性"
//     负向用例不能被 .NET 的宽容解析救活（.NET 对未知属性名放行）。
func hasFallbackTrigger(pattern string, r *RegExp) bool {
	if r.UnicodeSets {
		return false
	}
	deepOnly := r.Unicode // u 标志下只认"原理级"特征
	for i := 0; i < len(pattern); i++ {
		c := pattern[i]
		if c != '\\' {
			if c == '(' && i+1 < len(pattern) && pattern[i+1] == '?' {
				// (?<= / (?<! / (?ims-ims:
				if i+2 < len(pattern) {
					switch {
					case pattern[i+2] == '<' && i+3 < len(pattern) &&
						(pattern[i+3] == '=' || pattern[i+3] == '!'):
						return true
					case pattern[i+2] != '<' && pattern[i+2] != '=' &&
						pattern[i+2] != '!' && pattern[i+2] != ':' &&
						pattern[i+2] != '#' && pattern[i+2] != 'P':
						// (?X: 形态（X 非特殊引导符）→ 作用域修饰符组
						if !deepOnly && isModifierHeader(pattern[i+2:]) {
							return true
						}
					}
				}
			}
			continue
		}
		if i+1 >= len(pattern) {
			return false
		}
		n := pattern[i+1]
		if n == 'k' && i+2 < len(pattern) && pattern[i+2] == '<' {
			return true // \k<name>
		}
		if n >= '1' && n <= '9' {
			return true // 回溯引用
		}
		i++ // 跳过被转义字符
	}
	return false
}

// isModifierHeader 判断从 modifiers 位置开始的片段是否为 (?ims-ims: 组头
// （只看前缀里的合法修饰符字符与 '-'，遇到 ':' 才确认）。
func isModifierHeader(s string) bool {
	for j := 0; j < len(s) && j < 8; j++ {
		switch s[j] {
		case ':':
			return true
		case 'i', 'm', 's', '-':
			continue
		default:
			return false
		}
	}
	return false
}

// checkLookbehindNotQuantifiable: JS 里 lookbehind 不是 QuantifiableAssertion，
// (?<=…)? / (?<!…)* / (?<!…){2,3} 一律 SyntaxError（language/literals/regexp/
// invalid-*-lookbehind 8 例）。.NET 对零宽断言跟量词更宽容，必须在两条引擎
// 路径之前统一拦下。
func checkLookbehindNotQuantifiable(pattern string) error {
	for i := 0; i < len(pattern); i++ {
		if !strings.HasPrefix(pattern[i:], "(?<") || i+3 >= len(pattern) {
			continue
		}
		if pattern[i+3] != '=' && pattern[i+3] != '!' {
			continue
		}
		// 从 lookbehind 头开始做括号配对（跳过转义与字符类）
		depth := 1
		j := i + 4
		for j < len(pattern) && depth > 0 {
			c := pattern[j]
			switch c {
			case '\\':
				j++
			case '[':
				// 跳过字符类（[内] 的 ) 不参与配对）
				k := j + 1
				if k < len(pattern) && pattern[k] == '^' {
					k++
				}
				if k < len(pattern) && pattern[k] == ']' {
					k++
				}
				for k < len(pattern) && pattern[k] != ']' {
					if pattern[k] == '\\' {
						k++
					}
					k++
				}
				j = k
			case '(':
				depth++
			case ')':
				depth--
				if depth == 0 {
					if j+1 < len(pattern) {
						switch pattern[j+1] {
						case '?', '*', '+':
							return fmt.Errorf("lookbehind is not quantifiable")
						case '{':
							return fmt.Errorf("lookbehind is not quantifiable (range)")
						}
					}
					j = j + 1
					i = j // 继续外层扫描
					goto nextOuter
				}
			}
			j++
		}
		i = j
	nextOuter:
	}
	return nil
}
