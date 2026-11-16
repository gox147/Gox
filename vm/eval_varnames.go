package vm

// evalVarDeclaredNames 收集 eval 源码顶层的 var 声明名 (规范
// sec-performeval VarDeclaredNames 语义: var 穿透块与控制流提升, 但**不进入**
// 嵌套函数/箭头/类 —— 那是新函数作用域)。
//
// 用途: 直接 eval 的补充早错 (sec-evaldeclarationinstantiation 5.d.ii) ——
// 调用方函数为非简单形参列表时, eval 代码 varNames 含 "arguments" 即
// SyntaxError。实现在 vm 包 (有 parser/ast 可用), 经 object 桥注册给 stdlib
// 的 eval 内建 (与 CompileSourceEval 同纪律: stdlib 不依赖前端包)。
//
// 解析失败返回 nil —— eval 源码本身不合法, 后续编译会报 SyntaxError, 这里
// 不抢先判 (否则会改写错误的报错形态)。

import (
	"github.com/14752222/Gox/ast"
	"github.com/14752222/Gox/lexer"
	"github.com/14752222/Gox/parser"
)

func evalVarDeclaredNames(src string) []string {
	p := parser.New(lexer.New(src))
	program := p.ParseProgram()
	if p.Errors().HasErrors() {
		return nil
	}
	var names []string
	collectStmts(program.Statements, &names)
	return names
}

// collectStmts 收集一段语句表里的 var 声明名; depth 防御病态嵌套。
func collectStmts(stmts []ast.Statement, out *[]string) {
	for _, stmt := range stmts {
		if stmt == nil {
			continue
		}
		switch node := stmt.(type) {
		case *ast.VarStatement:
			if node.Name != nil {
				collectDeclarator(node.Name, node.Value, out)
			}
			for _, d := range node.More {
				collectDeclarator(d.Name, d.Value, out)
			}
		case *ast.BlockStatement:
			collectStmts(node.Statements, out)
		case *ast.IfStatement:
			if node.Consequence != nil {
				collectStmts(node.Consequence.Statements, out)
			}
			if node.Alternative != nil {
				collectStmts(node.Alternative.Statements, out)
			}
		case *ast.ForStatement:
			if node.Init != nil {
				collectStmts([]ast.Statement{node.Init}, out)
			}
			if node.Body != nil {
				collectStmts(node.Body.Statements, out)
			}
		case *ast.ForOfStatement:
			if node.VarDecl != nil {
				collectStmts([]ast.Statement{node.VarDecl}, out)
			}
			if node.Body != nil {
				collectStmts(node.Body.Statements, out)
			}
		case *ast.ForInStatement:
			if node.VarDecl != nil {
				collectStmts([]ast.Statement{node.VarDecl}, out)
			}
			if node.Body != nil {
				collectStmts(node.Body.Statements, out)
			}
		case *ast.WhileStatement:
			if node.Body != nil {
				collectStmts(node.Body.Statements, out)
			}
		case *ast.DoWhileStatement:
			if node.Body != nil {
				collectStmts(node.Body.Statements, out)
			}
		case *ast.WithStatement:
			if node.Body != nil {
				collectStmts([]ast.Statement{node.Body}, out)
			}
		case *ast.LabeledStatement:
			if node.Body != nil {
				collectStmts([]ast.Statement{node.Body}, out)
			}
		case *ast.TryStatement:
			if node.Body != nil {
				collectStmts(node.Body.Statements, out)
			}
			if node.CatchBody != nil {
				collectStmts(node.CatchBody.Statements, out)
			}
			if node.FinallyBody != nil {
				collectStmts(node.FinallyBody.Statements, out)
			}
		case *ast.SwitchStatement:
			for _, c := range node.Cases {
				collectStmts(c.Statements, out)
			}
		}
	}
}

// destructureSyntheticName 是解构声明项在 AST 中的合成名 (与
// compiler/parser 两侧同口径 —— 该常量未跨包导出, 这里按字面值对齐)。
const destructureSyntheticName = "__destructure__"

// collectDeclarator 收集一个声明项: 简单名直接收; 解构项 (Name 是合成名)
// 收模式里的真实绑定名 (var 解构同样提升, 与 compiler.collectVarDeclaratorNames
// 同口径)。
func collectDeclarator(name *ast.Identifier, value ast.Expression, out *[]string) {
	if name == nil {
		return
	}
	if name.Value == destructureSyntheticName {
		*out = append(*out, ast.PatternBoundNames(value)...)
		return
	}
	*out = append(*out, name.Value)
}
