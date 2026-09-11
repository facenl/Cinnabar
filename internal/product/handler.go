// internal/product/handler.go
package product

import (
	"errors"
	"strconv"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"cinnabar/pkg/errcode"
	"cinnabar/pkg/response"
)

// Handler 只做三件事：参数绑定/解析、调用 service、按统一契约输出。
// 业务规则一律下沉 service，保证 handler 薄到可以凭类型签名读懂。
type Handler struct {
	svc *Service
	log *zap.Logger
}

func NewHandler(svc *Service, log *zap.Logger) *Handler {
	return &Handler{svc: svc, log: log}
}

// RegisterRoutes 挂载到 /api/v1 组之下（认证/限流中间件由 main 装配在该组上）。
func (h *Handler) RegisterRoutes(rg *gin.RouterGroup) {
	g := rg.Group("/products")
	g.POST("", h.Create)
	g.GET("", h.List)
	g.GET("/:id", h.GetByID)
	g.PUT("/:id", h.Update)
	g.DELETE("/:id", h.Delete)
	g.POST("/:id/deduct", h.DeductStock)
}

// parseID 路径参数统一入口：非法 id 一律按参数错误处理，
// 不把它漏到 service 变成「查无此物」的 404（语义不同）。
func (h *Handler) parseID(c *gin.Context) (uint64, bool) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil || id == 0 {
		response.Fail(c, errcode.ErrInvalidParam, "invalid product id")
		return 0, false
	}
	return id, true
}

// handleErr 错误翻译收口：BizError → 对应错误码；
// 未知错误 → 500 兜底（日志带 request_id，响应不回显内部细节防信息泄露）。
func (h *Handler) handleErr(c *gin.Context, action string, err error) {
	var be *BizError
	if errors.As(err, &be) {
		response.Fail(c, be.Code, be.Msg)
		return
	}
	h.log.Error("internal error",
		zap.String("action", action),
		zap.String("request_id", response.RequestIDFromContext(c.Request.Context())),
		zap.Error(err),
	)
	response.Fail(c, errcode.ErrInternal, "internal server error")
}

// Create POST /api/v1/products
func (h *Handler) Create(c *gin.Context) {
	var req ProductCreateReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Fail(c, errcode.ErrInvalidParam, err.Error())
		return
	}
	p, err := h.svc.Create(c.Request.Context(), &req)
	if err != nil {
		h.handleErr(c, "create_product", err)
		return
	}
	response.Success(c, NewProductResp(p))
}

// GetByID GET /api/v1/products/:id
func (h *Handler) GetByID(c *gin.Context) {
	id, ok := h.parseID(c)
	if !ok {
		return
	}
	p, err := h.svc.GetByID(c.Request.Context(), id)
	if err != nil {
		h.handleErr(c, "get_product", err)
		return
	}
	response.Success(c, NewProductResp(p))
}

// Update PUT /api/v1/products/:id
func (h *Handler) Update(c *gin.Context) {
	id, ok := h.parseID(c)
	if !ok {
		return
	}
	var req ProductUpdateReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Fail(c, errcode.ErrInvalidParam, err.Error())
		return
	}
	p, err := h.svc.Update(c.Request.Context(), id, &req)
	if err != nil {
		h.handleErr(c, "update_product", err)
		return
	}
	response.Success(c, NewProductResp(p))
}

// Delete DELETE /api/v1/products/:id
func (h *Handler) Delete(c *gin.Context) {
	id, ok := h.parseID(c)
	if !ok {
		return
	}
	if err := h.svc.Delete(c.Request.Context(), id); err != nil {
		h.handleErr(c, "delete_product", err)
		return
	}
	response.Success(c, gin.H{"id": id, "deleted": true})
}

// List GET /api/v1/products?cursor=&limit=&status=&category_id=&keyword=
func (h *Handler) List(c *gin.Context) {
	var q ProductListQuery
	if err := c.ShouldBindQuery(&q); err != nil {
		response.Fail(c, errcode.ErrInvalidParam, err.Error())
		return
	}
	res, err := h.svc.List(c.Request.Context(), &q)
	if err != nil {
		h.handleErr(c, "list_products", err)
		return
	}
	items := make([]*ProductResp, 0, len(res.Items))
	for _, p := range res.Items {
		items = append(items, NewProductResp(p))
	}
	response.Success(c, &ProductListResp{
		Items:      items,
		NextCursor: res.NextCursor,
		HasMore:    res.HasMore,
	})
}

// DeductStock POST /api/v1/products/:id/deduct
func (h *Handler) DeductStock(c *gin.Context) {
	id, ok := h.parseID(c)
	if !ok {
		return
	}
	var req DeductStockReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Fail(c, errcode.ErrInvalidParam, err.Error())
		return
	}
	p, err := h.svc.DeductStock(c.Request.Context(), id, req.Quantity)
	if err != nil {
		h.handleErr(c, "deduct_stock", err)
		return
	}
	response.Success(c, NewProductResp(p))
}
