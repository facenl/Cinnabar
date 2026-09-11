// pkg/errcode/errcode.go
package errcode

import "net/http"

// 业务错误码按「HTTP 类别 + 两位序号」编排：
//   - 前 3 位与 HTTP 语义对齐，网关/监控可直接按段聚合；
//   - 后 2 位留给业务细分，避免与 HTTP 状态码互相污染。
const (
	ErrInvalidParam          = 40001 // 参数校验失败
	ErrUnauthorized          = 40101 // 未认证 / 凭证无效
	ErrForbidden             = 40301 // 已认证但无权限（预留给渠道 scope 控制）
	ErrTooManyRequests       = 42901 // 触发限流
	ErrProductNotFound       = 40401 // 商品不存在（含已软删除）
	ErrProductStockNotEnough = 40901 // 库存不足
	ErrProductUpdateConflict = 40902 // 乐观锁冲突
	ErrInternal              = 50000 // 未分类的内部错误（兜底）
)

// HTTPStatus 把业务错误码映射回 HTTP 状态码。
// 为什么需要这层映射：REST 语义（CDN/网关/浏览器缓存都看 HTTP 状态）
// 与业务码（前端/渠道方精细处理）解耦，两处各自演进互不影响。
func HTTPStatus(code int) int {
	switch code {
	case ErrInvalidParam:
		return http.StatusBadRequest
	case ErrUnauthorized:
		return http.StatusUnauthorized
	case ErrForbidden:
		return http.StatusForbidden
	case ErrTooManyRequests:
		return http.StatusTooManyRequests
	case ErrProductNotFound:
		return http.StatusNotFound
	case ErrProductStockNotEnough, ErrProductUpdateConflict:
		return http.StatusConflict
	default:
		return http.StatusInternalServerError
	}
}

// Message 提供默认文案，调用方仍可按场景覆盖为更具体的提示。
func Message(code int) string {
	switch code {
	case ErrInvalidParam:
		return "invalid parameter"
	case ErrUnauthorized:
		return "unauthorized"
	case ErrForbidden:
		return "forbidden"
	case ErrTooManyRequests:
		return "too many requests"
	case ErrProductNotFound:
		return "product not found"
	case ErrProductStockNotEnough:
		return "product stock not enough"
	case ErrProductUpdateConflict:
		return "product update conflict"
	default:
		return "internal server error"
	}
}
