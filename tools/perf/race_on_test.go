//go:build race

package perf

// raceDetectorEnabled 供分配预算闸门跳过 -race 构建。
//
// 为什么必须跳：-race 会在每次 map/slice 访问上插桩，并给运行时对象加影子内存，
// 分配次数会整体漂移（不是"多一点"，是会变），拿它去比 408,000 这种精确预算
// 只会得到假红。CI 里没有 -race 档（零 cgo 硬约束，见 ci.yml），但开发者本地
// 会跑，所以判据本身要认得出这种口径。
const raceDetectorEnabled = true
