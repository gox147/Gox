package vm

import "testing"

const fib28ProbeSrc = `
function fib(n) { if (n < 2) return n; return fib(n - 1) + fib(n - 2); }
let t = 0;
function add(a, b) { return a + b; }
for (let i = 0; i < 100000; i++) { t = add(t, i); }
fib(28) + t;
`

func BenchmarkFib28Probe(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := EvalVM(fib28ProbeSrc); err != nil {
			b.Fatal(err)
		}
	}
}
