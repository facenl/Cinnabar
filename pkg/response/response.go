// pkg/response/response.go
package response

import (
	"context"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"cinnabar/pkg/errcode"
)

// CtxKeyRequestID 是 request_id 在 gin.Context 中的约定键，
// 由 middleware.RequestID 写入，本包读取——两处必须保持一致。
const CtxKeyRequestID = "request_id"

// apiVersion 写入每个响应的 meta：无头架构下前端/渠道方需要
// 显式感知契约版本，为将来 /api/v2 并存期留好识别位。
const apiVersion = "v1"

// ctxKey 是 request_id 在 request.Context 中的私有键，
// 与 gin.Context 双写是为了：service/repository 层只持有 ctx 时
// 打日志也能带上 request_id，不必反向依赖 gin。
type ctxKey struct{}

// ContextWithRequestID 把 request_id 写入标准 context（供中间件调用）。
func ContextWithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxKey{}, id)
}

// RequestIDFromContext 从标准 context 中取 request_id，取不到返回空串。
func RequestIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(ctxKey{}).(string); ok {
		return v
	}
	return ""
}

// Meta 是所有响应都携带的元信息：排障定位靠 request_id，兼容性靠 version。
type Meta struct {
	RequestID string `json:"request_id"`
	Version   string `json:"version"`
	Timestamp int64  `json:"timestamp"`
}

// ErrItem 是单条错误。用数组而不是单对象，是为参数校验
// 「一次返回多个字段错误」预留扩展位，契约保持稳定。
type ErrItem struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func buildMeta(c *gin.Context) Meta {
	v, _ := c.Get(CtxKeyRequestID)
	id, _ := v.(string)
	return Meta{
		RequestID: id,
		Version:   apiVersion,
		Timestamp: time.Now().Unix(),
	}
}

// Success 统一成功出口：{"data": ..., "meta": ...}
func Success(c *gin.Context, data any) {
	c.JSON(http.StatusOK, gin.H{
		"data": data,
		"meta": buildMeta(c),
	})
}

// Fail 统一失败出口：{"errors": [...], "meta": ...}
// HTTP 状态码由错误码映射得出，并 Abort 掉后续 handler，防止重复写响应。
func Fail(c *gin.Context, code int, msg string) {
	if msg == "" {
		msg = errcode.Message(code)
	}
	c.AbortWithStatusJSON(errcode.HTTPStatus(code), gin.H{
		"errors": []ErrItem{{Code: code, Message: msg}},
		"meta":   buildMeta(c),
	})
}
