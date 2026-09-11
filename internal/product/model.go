// internal/product/model.go
package product

import (
	"time"

	"gorm.io/gorm"
)

// 商品上下架状态。用 int8 而不是 bool：后续可能扩展
// 「预售/冻结/清仓」等中间态，布尔会逼出破坏性迁移。
const (
	StatusOff int8 = 0 // 下架
	StatusOn  int8 = 1 // 上架
)

// Product 是 GORM 实体，只带 gorm 标签，刻意不带 json 标签。
// 契约优先原则：DB 结构与 API 契约是两条独立演进的曲线——
// 表结构变更（加列/改名/拆分）不应直接冲击对外 JSON 契约，
// 二者之间的翻译由 handler 层的 DTO 转换完成。
type Product struct {
	ID          uint64         `gorm:"primaryKey;autoIncrement"`
	Name        string         `gorm:"type:varchar(128);uniqueIndex;not null"`
	Description string         `gorm:"type:text"`
	Price       int64          `gorm:"not null;default:0"`         // 单位：分。金额用整型避免 float 二进制误差
	Stock       int32          `gorm:"not null;default:0"`         // 库存，扣减走行锁+乐观锁
	CategoryID  uint64         `gorm:"index;not null;default:0"`   // 列表高频过滤条件，必须建索引
	Status      int8           `gorm:"not null;default:1"`         // 1=上架 0=下架
	Version     int32          `gorm:"not null;default:1"`         // 乐观锁版本号，每次写 +1
	CreatedAt   time.Time      `gorm:"not null"`
	UpdatedAt   time.Time      `gorm:"not null"`
	DeletedAt   gorm.DeletedAt `gorm:"index"` // 软删除：无头电商商品被多渠道引用，物理删除会留下悬空引用
}

func (Product) TableName() string { return "products" }
