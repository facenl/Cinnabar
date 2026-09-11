// internal/middleware/clock.go
package middleware

import "time"

// nowUnix 包级时钟变量：单测可替换，业务代码无需注入 Clock 接口。
var nowUnix = func() int64 { return time.Now().Unix() }
