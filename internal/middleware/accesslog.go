// internal/middleware/accesslog.go
package middleware

import (
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"cinnabar/pkg/response"
)

// AccessLog 结构化访问日志。关键字段：
//   - request_id：串联同一次请求的所有日志行；
//   - identity_*：区分前端用户与渠道方，对账/限流排查必备；
//   - latency / status：慢请求与错误率的原始素材。
//
// 生产环境需补充：Prometheus 指标埋点位置——建议在此处（中间件收口点）
// 按 route+status+identity_type 维度记录 http_requests_total /
// http_request_duration_seconds，比散落在 handler 里完整且无侵入。
func AccessLog(log *zap.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()

		idType, _ := c.Get(CtxIdentityType)
		idID, _ := c.Get(CtxIdentityID)
		log.Info("http access",
			zap.String("request_id", response.RequestIDFromContext(c.Request.Context())),
			zap.String("method", c.Request.Method),
			zap.String("path", c.Request.URL.Path),
			zap.Int("status", c.Writer.Status()),
			zap.Duration("latency", time.Since(start)),
			zap.String("client_ip", c.ClientIP()),
			zap.Any("identity_type", idType),
			zap.Any("identity_id", idID),
		)
	}
}
