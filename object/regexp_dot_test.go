package object

import "testing"

// T2 第三层 (rvDg77, 2026-11-18): 非 u 模式裸 '.' 的单码元物化 +
// 修饰符组 dotAll 作用域。表驱动断言经由引擎完整路径 (NewRegExp) 验证
// 模式合法性与匹配行为, 不直戳内部转换函数。
func TestJSDotMaterialization(t *testing.T) {
	tests := []struct {
		name    string
		pattern string
		flags   string
		input   string
		want    bool
	}{
		// 码元语义: astral 字符是两个码元, 单个 '.' 吃不下
		{"dotAll 组不吃 astral", `(?s:^.$)`, "", "\U00010300", false},
		{"裸点不吃 astral", `^.$`, "", "\U00010300", false},
		{"dotAll 组吃 BMP", `(?s:^.$)`, "", "a", true},
		{"裸点吃 BMP", `^.$`, "", "a", true},
		// 行终止符: 非 s 时 JS 还排除 \r \u2028 \u2029 (RE2 默认只排 \n)
		{"裸点排 \r", `.`, "", "\r", false},
		{"裸点排 U+2028", `.`, "", " ", false},
		{"dotAll 点吃 U+2028", `(?s:.)`, "", " ", true},
		{"dotAll 点吃 \n", `(?s:.)`, "", "\n", true},
		{"非 s 点不吃 \n", `(?-s:.)`, "s", "\n", false},
		// 修饰符组只影响组内: 全局 s 不影响 (?-s:…) 内的点
		{"组内减 s 覆盖全局", `(?-s:c.c)`, "s", "c\nc", false},
		{"组外加号不受组内减 s 影响", `a.a|(?-s:c.c)`, "s", "a\na", true},
		{"组内加 s 覆盖全局", `(?s:.e)`, "", "\ne", true},
		// 标志减除降级: (?-i:…) 走 regexp2
		{"组内减 i", `(?-i:.es)`, "i", "aeS", false},
		{"组内减 i 保持精确匹配", `(?-i:.es)`, "i", "aes", true},
		// 字面点 / 类内点不受影响
		{"转义点是字面", `\.`, "", ".", true},
		{"类内点是字面", `[.]`, "", ".", true},
		{"类内点不吃换行语义", `[.]`, "", "\n", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, err := NewRegExp(tt.pattern, tt.flags)
			if err != nil {
				t.Fatalf("NewRegExp(%q,%q): %v", tt.pattern, tt.flags, err)
			}
			var got bool
			if r.Regexp != nil {
				got = r.Regexp.MatchString(tt.input)
			} else if r.Regexp2 != nil {
				m, _ := r.Regexp2.MatchString(tt.input)
				got = m
			}
			if got != tt.want {
				t.Errorf("/%s/%s on %q = %v, want %v", tt.pattern, tt.flags, tt.input, got, tt.want)
			}
		})
	}
}

// 脚本四字母码别名 (T2 第三层补齐): \p{Script=Beng} 此前 SyntaxError。
func TestScriptAliasCompletion(t *testing.T) {
	tests := []struct{ content string }{
		{`\p{Script=Beng}`}, {`\p{Script_Extensions=Beng}`},
		{`\p{Script=Brah}`}, {`\p{Script=Hira}`}, {`\p{Script=Knda}`},
		{`\p{Script=Mlym}`}, {`\p{Script=Bali}`}, {`\p{Script=Nbat}`},
		{`\p{Script=Sind}`},
	}
	for _, tt := range tests {
		t.Run(tt.content, func(t *testing.T) {
			if _, err := NewRegExp(`^`+tt.content+`+$`, "u"); err != nil {
				t.Errorf("%s: %v", tt.content, err)
			}
		})
	}
}
