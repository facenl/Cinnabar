// internal/middleware/requestid.go
package middleware

import (
	"crypto/rand"
	"encoding/hex"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"cinnabar/pkg/response"
)

// RequestID 请求标识中间件，必须挂在链路最前面（Recovery 之后、业务之前），
// 否则前面的日志/响应拿不到 request_id。
//
// 信任客户端传入的 X-Request-ID：前端/网关常常已经在入口生成过，
// 透传它可以把客户端日志与服务端日志串成一条线。
func RequestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		rid := strings.TrimSpace(c.GetHeader("X-Request-ID"))
		if rid == "" || len(rid) > 64 { // 过长视为异常输入，直接换新的
			rid = newRequestID()
		}
		// 双写：gin.Context 供 response 包读；request.Context 供
		// service/repository 层读（它们不依赖 gin）。
		c.Set(response.CtxKeyRequestID, rid)
		c.Request = c.Request.WithContext(
			response.ContextWithRequestID(c.Request.Context(), rid))
		c.Header("X-Request-ID", rid) // 回写给客户端，便于客服/排障报障
		c.Next()
	}
}

// newRequestID 用 crypto/rand 生成 16 字节十六进制串。
// 不引入第三方 uuid 库：request_id 只要唯一可读即可，不需要 uuid 的版本语义。
func newRequestID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand 失败的极端场景退化为纳秒时间戳，可用性优先。
		return "ts-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(b)
}
