//go:build race

package gfx

// raceDetectorEnabled 报"本次测试是否在 -race 下跑"。
//
// 用途只有一处: 成本类断言 (墙钟) 在 race 检测下必然慢一个数量级 (实测族索引
// 14ms → 117ms), 那不是回归, 是插桩。这类断言据此跳过, 而不是把阈值放宽到
// 失去意义的程度 —— 放宽阈值等于让闸门永远绿。
const raceDetectorEnabled = true
