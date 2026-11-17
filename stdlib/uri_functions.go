package stdlib

// URI 编解码全局函数: encodeURI / decodeURI / encodeURIComponent /
// decodeURIComponent (ECMA-262 §19.2.6.1-19.2.6.4)。
//
// 根因单 rxDOIl [T1]: 此前全局从未注册这四个函数 (typeof encodeURI ===
// 'undefined'), built-ins/encodeURI|decodeURI* 四族 ~87 条全灭。实现按规范
// Encode / Decode 抽象操作逐条对齐, 不借 net/url —— Go 的 url 包对保留字符
// 集、错误类型 (URIError vs TypeError) 与不解码保留字符的行为均与规范不符。
//
// 关键规范点:
//   - encodeURI: 未转义集 = 字母数字 + "-_.!~*'()" + 保留 ";/?:@&=+$,#" +
//     '#'(仅 encodeURI); encodeURIComponent 不保留 ";/?:@&=+$,#"。
//   - 孤立代理 / 非法 UTF-8 → URIError (encodeURI-\uD800 族)。
//   - decodeURI: 单字节且落在保留集 (含 '#') 的 %XX 不解码; 多字节序列按
//     UTF-8 严格表校验 (含 0xED 0x80-0x9F 排除代理区), 拼不成合法序列 →
//     URIError (overlong "%C0%80"、"%E0%80%80" 等用例)。

import (
	"strings"

	"github.com/14752222/Gox/object"
	"github.com/14752222/Gox/runtime"
)

// uriReservedSet: 规范 URIRemainingSet/URIReservedSet, 含 '#' (19.2.6.1)。
var uriReservedSet = map[byte]bool{
	';': true, '/': true, '?': true, ':': true, '@': true, '&': true,
	'=': true, '+': true, '$': true, ',': true, '#': true,
}

// uriUnescapedSet: 规范 URIUnescapedSet 中的标点部分 (字母数字单判)。
var uriUnescapedSet = map[byte]bool{
	'-': true, '_': true, '.': true, '!': true, '~': true, '*': true,
	'\'': true, '(': true, ')': true,
}

func isURIAlphaNum(b byte) bool {
	return (b >= '0' && b <= '9') || (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z')
}

const hexUpper = "0123456789ABCDEF"

// utf8SeqLen 按规范 UTF-8 首字节表返回期望序列长度; 非法首字节返回 0
// (0x80-0xBF 孤立续字节、0xC0/0xC1 overlong、>0xF4 均不合法)。
func utf8SeqLen(b0 byte) int {
	switch {
	case b0 < 0x80:
		return 1
	case b0 >= 0xC2 && b0 <= 0xDF:
		return 2
	case b0 >= 0xE0 && b0 <= 0xEF:
		return 3
	case b0 >= 0xF0 && b0 <= 0xF4:
		return 4
	}
	return 0
}

// validUTF8Bytes 对一段完整字节序列做严格 UTF-8 校验 (ECMA-262 Decode 步骤 11
// 的范围表: 0xE0→A0-BF, 0xED→80-9F, 0xF0→90-BF, 0xF4→80-8F, 其余 80-BF)。
func validUTF8Bytes(b []byte) bool {
	if len(b) == 0 {
		return false
	}
	n := utf8SeqLen(b[0])
	if n != len(b) {
		return false
	}
	if n == 1 {
		return true
	}
	c := b[1]
	switch {
	case n == 2: // 首字节 0xC2-0xDF (utf8SeqLen 已排除 0xC0/0xC1)
		if c < 0x80 || c > 0xBF {
			return false
		}
	case b[0] == 0xE0:
		if c < 0xA0 || c > 0xBF {
			return false
		}
	case (b[0] >= 0xE1 && b[0] <= 0xEC) || (b[0] >= 0xEE && b[0] <= 0xEF):
		if c < 0x80 || c > 0xBF {
			return false
		}
	case b[0] == 0xED:
		if c < 0x80 || c > 0x9F {
			return false // 排除 U+D800-DFFF 代理区
		}
	case b[0] == 0xF0:
		if c < 0x90 || c > 0xBF {
			return false
		}
	case b[0] >= 0xF1 && b[0] <= 0xF3:
		if c < 0x80 || c > 0xBF {
			return false
		}
	case b[0] == 0xF4:
		if c < 0x80 || c > 0x8F {
			return false
		}
	default:
		return false // 0xC0/0xC1 等非法首字节
	}
	for k := 2; k < n; k++ {
		if b[k] < 0x80 || b[k] > 0xBF {
			return false
		}
	}
	return true
}

// encodeURIText 实现规范 Encode。keepReserved=true 为 encodeURI (保留字符与
// '#' 原样输出); false 为 encodeURIComponent (保留字符也编码)。
// 规范对孤立代理抛 URIError; 引擎把 \uD800 类转义存为非法 UTF-8 序列,
// 严格表校验天然拒绝, 两类表示殊途同归。
func encodeURIText(s string, keepReserved bool) (string, *object.Error) {
	var out strings.Builder
	for i := 0; i < len(s); {
		b := s[i]
		if b < 0x80 {
			if isURIAlphaNum(b) || uriUnescapedSet[b] || (keepReserved && uriReservedSet[b]) {
				out.WriteByte(b)
			} else {
				out.WriteByte('%')
				out.WriteByte(hexUpper[b>>4])
				out.WriteByte(hexUpper[b&0x0F])
			}
			i++
			continue
		}
		// 非首字节: 找出完整 UTF-8 序列再逐字节转义
		n := utf8SeqLen(b)
		if n == 0 || i+n > len(s) || !validUTF8Bytes([]byte(s[i:i+n])) {
			return "", object.NewErrorWithName("URIError", "URI malformed")
		}
		for k := 0; k < n; k++ {
			eb := s[i+k]
			out.WriteByte('%')
			out.WriteByte(hexUpper[eb>>4])
			out.WriteByte(hexUpper[eb&0x0F])
		}
		i += n
	}
	return out.String(), nil
}

// hexByteAt 取 s[i:i+3] 形如 "%XX" 的字节; 不构成转义返回 false。
func hexByteAt(s string, i int) (byte, bool) {
	if i+3 > len(s) || s[i] != '%' {
		return 0, false
	}
	hi, lo := s[i+1], s[i+2]
	var hv, lv int
	switch {
	case hi >= '0' && hi <= '9':
		hv = int(hi - '0')
	case hi >= 'A' && hi <= 'F':
		hv = int(hi-'A') + 10
	case hi >= 'a' && hi <= 'f':
		hv = int(hi-'a') + 10
	default:
		return 0, false
	}
	switch {
	case lo >= '0' && lo <= '9':
		lv = int(lo - '0')
	case lo >= 'A' && lo <= 'F':
		lv = int(lo-'A') + 10
	case lo >= 'a' && lo <= 'f':
		lv = int(lo-'a') + 10
	default:
		return 0, false
	}
	return byte(hv<<4 | lv), true
}

// decodeURIText 实现规范 Decode。preserveReserved=true 为 decodeURI
// (单字节落在保留集的 %XX 不解码); false 为 decodeURIComponent (全解)。
func decodeURIText(s string, preserveReserved bool) (string, *object.Error) {
	uriErr := func() *object.Error {
		return object.NewErrorWithName("URIError", "URI malformed")
	}
	var out strings.Builder
	for i := 0; i < len(s); {
		b := s[i]
		if b != '%' {
			if b >= 0x80 {
				// 字面非 ASCII 字符: 引擎内部串是合法 UTF-8, 逐序列透传
				n := utf8SeqLen(b)
				if n == 0 || i+n > len(s) || !validUTF8Bytes([]byte(s[i:i+n])) {
					return "", uriErr()
				}
				out.WriteString(s[i : i+n])
				i += n
				continue
			}
			out.WriteByte(b)
			i++
			continue
		}
		first, ok := hexByteAt(s, i)
		if !ok {
			return "", uriErr()
		}
		if first < 0x80 {
			if preserveReserved && uriReservedSet[first] {
				// 规范 Decode 步骤 10b: 保留字符不解码, 原样 %XX
				out.WriteString(s[i : i+3])
			} else {
				out.WriteByte(first)
			}
			i += 3
			continue
		}
		// 多字节序列: 收集 k 个 %XX 再整体按严格表校验
		n := utf8SeqLen(first)
		if n == 0 {
			return "", uriErr()
		}
		seq := make([]byte, 0, n)
		seq = append(seq, first)
		j := i + 3
		for k := 1; k < n; k++ {
			nb, ok := hexByteAt(s, j)
			if !ok {
				return "", uriErr()
			}
			seq = append(seq, nb)
			j += 3
		}
		if !validUTF8Bytes(seq) {
			return "", uriErr()
		}
		out.Write(seq)
		i = j
	}
	return out.String(), nil
}

// setupURIFunctions 注册全局 encodeURI / decodeURI / encodeURIComponent /
// decodeURIComponent (规范 19.2.6, length 均 1, 不带 [[Construct]])。
// 由 stdlib.go 在 setupNumberFunctions 之后调用。
func setupURIFunctions(env *runtime.Environment) {
	registerURIFn := func(name string, encode, keep bool) {
		env.Declare(name, object.NewBuiltin(name, func(args ...object.Value) object.Value {
			src := ""
			if len(args) > 0 {
				src = toStr(args[0])
			}
			var res string
			var err *object.Error
			if encode {
				res, err = encodeURIText(src, keep) // encodeURI / encodeURIComponent
			} else {
				res, err = decodeURIText(src, keep) // decodeURI / decodeURIComponent
			}
			if err != nil {
				return err
			}
			return object.NewString(res)
		}), false)
	}
	registerURIFn("encodeURI", true, true)
	registerURIFn("decodeURI", false, true)
	registerURIFn("encodeURIComponent", true, false)
	registerURIFn("decodeURIComponent", false, false)
}
