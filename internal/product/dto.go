// internal/product/dto.go
package product

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// 分页边界常量：防止客户端传 limit=10000 拖垮 DB。
const (
	defaultLimit = 20
	maxLimit     = 100
)

// ErrBadCursor 游标解码失败，service 层会翻译为 ErrInvalidParam。
var ErrBadCursor = errors.New("invalid cursor")

// ---------- 请求 DTO ----------

// ProductCreateReq 创建请求。Status 用指针以区分「未传（走默认上架）」
// 与「显式传 0（下架）」——这是 int8 零值无法表达的语义。
type ProductCreateReq struct {
	Name        string `json:"name" binding:"required,max=128"`
	Description string `json:"description" binding:"max=2000"`
	Price       int64  `json:"price" binding:"required,gte=0"` // 单位：分
	Stock       int32  `json:"stock" binding:"gte=0"`
	CategoryID  uint64 `json:"category_id" binding:"required"`
	Status      *int8  `json:"status" binding:"omitempty,oneof=0 1"`
}

// ProductUpdateReq 更新请求。全部字段指针化：PUT 在本契约里按
// 「部分更新」语义实现（只更新传了的字段），这是无头前端最常见的用法。
// Version 必传——乐观锁的前提就是客户端先读后写、带回读到的版本号。
type ProductUpdateReq struct {
	Name        *string `json:"name" binding:"omitempty,max=128"`
	Description *string `json:"description" binding:"omitempty,max=2000"`
	Price       *int64  `json:"price" binding:"omitempty,gte=0"`
	Stock       *int32  `json:"stock" binding:"omitempty,gte=0"`
	CategoryID  *uint64 `json:"category_id" binding:"omitempty"`
	Status      *int8   `json:"status" binding:"omitempty,oneof=0 1"`
	Version     int32   `json:"version" binding:"required,gt=0"` // 乐观锁版本号
}

// ProductListQuery 列表查询。cursor-based 而非 offset：
// 商品列表是持续写入的，offset 分页在翻页过程中遇到新增/删除会重会漏；
// 游标锚定 (created_at, id) 物理位置，翻页期间数据变动也不影响已翻过的页。
type ProductListQuery struct {
	Cursor     string `form:"cursor"`
	Limit      int    `form:"limit" binding:"omitempty,min=1,max=100"`
	Status     *int8  `form:"status" binding:"omitempty,oneof=0 1"`
	CategoryID uint64 `form:"category_id" binding:"omitempty"`
	Keyword    string `form:"keyword" binding:"omitempty,max=64"`
}

// DeductStockReq 扣减库存请求。
type DeductStockReq struct {
	Quantity int32 `json:"quantity" binding:"required,gt=0"`
}

// ---------- 响应 DTO ----------

// ProductResp 对外契约。与 Model 字段看似重复，但这是有意的解耦：
// 将来 Model 加列（如 cost_price 成本价）不会意外泄露到 API。
type ProductResp struct {
	ID          uint64    `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Price       int64     `json:"price"`
	Stock       int32     `json:"stock"`
	CategoryID  uint64    `json:"category_id"`
	Status      int8      `json:"status"`
	Version     int32     `json:"version"` // 透出给前端，更新时需带回
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// NewProductResp Model -> DTO 的转换收口在这一处，禁止 handler 里散落拼装。
func NewProductResp(p *Product) *ProductResp {
	if p == nil {
		return nil
	}
	return &ProductResp{
		ID:          p.ID,
		Name:        p.Name,
		Description: p.Description,
		Price:       p.Price,
		Stock:       p.Stock,
		CategoryID:  p.CategoryID,
		Status:      p.Status,
		Version:     p.Version,
		CreatedAt:   p.CreatedAt,
		UpdatedAt:   p.UpdatedAt,
	}
}

// ProductListResp 游标分页响应。
type ProductListResp struct {
	Items      []*ProductResp `json:"items"`
	NextCursor string         `json:"next_cursor"`
	HasMore    bool           `json:"has_more"`
}

// ---------- 游标编解码 ----------

// EncodeCursor 把 (created_at, id) 编码为不透明 base64 串。
// 为什么带 id：created_at 精度内可能有多条记录（批量导入/同事务写入），
// 单靠时间戳分页会出现跨页重复或丢失；id 作为第二排序键保证全序唯一。
// 为什么不透明：游标是实现细节，编码后前端不会依赖其内部结构，将来可换格式。
func EncodeCursor(t time.Time, id uint64) string {
	raw := fmt.Sprintf("%d_%d", t.UnixNano(), id)
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// DecodeCursor 还原游标。任何格式错误都视为非法入参而不是内部错误。
func DecodeCursor(s string) (time.Time, uint64, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return time.Time{}, 0, ErrBadCursor
	}
	parts := strings.SplitN(string(raw), "_", 2)
	if len(parts) != 2 {
		return time.Time{}, 0, ErrBadCursor
	}
	nano, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return time.Time{}, 0, ErrBadCursor
	}
	id, err := strconv.ParseUint(parts[1], 10, 64)
	if err != nil {
		return time.Time{}, 0, ErrBadCursor
	}
	return time.Unix(0, nano), id, nil
}
