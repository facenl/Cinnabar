// internal/product/repository.go
package product

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// 仓储层哨兵错误。service 层据此映射业务错误码，
// 不把 gorm 原生错误透出到 handler——持久化实现是可替换的。
var (
	ErrStockNotEnough  = errors.New("stock not enough")
	ErrVersionConflict = errors.New("version conflict")
)

// ListFilter 列表过滤条件。Limit 为业务页大小，
// 实现内部实际取 Limit+1 条用于判断 has_more（避免额外 COUNT 查询）。
type ListFilter struct {
	Status     *int8
	CategoryID uint64
	Keyword    string

	HasCursor       bool      // 区分首页与后续页：首页不带游标条件
	CursorCreatedAt time.Time // 游标锚点
	CursorID        uint64
	Limit           int
}

// Repository 商品仓储接口。所有方法接收 ctx：
// 1) 让请求取消能传导到 SQL 层；2) 便于 trace/日志字段贯穿。
type Repository interface {
	Create(ctx context.Context, p *Product) error
	// GetByID 未命中返回 gorm.ErrRecordNotFound（含已软删除，GORM 自动过滤）。
	GetByID(ctx context.Context, id uint64) (*Product, error)
	// UpdateFieldsWithVersion 乐观锁部分更新，返回受影响行数。
	// 0 行 = 记录不存在或版本不匹配，由 service 层进一步区分。
	UpdateFieldsWithVersion(ctx context.Context, id uint64, version int32, fields map[string]any) (int64, error)
	// SoftDelete 软删除，返回受影响行数（0 = 不存在）。
	SoftDelete(ctx context.Context, id uint64) (int64, error)
	List(ctx context.Context, f ListFilter) ([]*Product, error)
	// DeductStock 事务内行锁 + 乐观锁扣减库存，返回扣减后的最新实体。
	DeductStock(ctx context.Context, id uint64, qty int32) (*Product, error)
}

type mysqlRepository struct {
	db *gorm.DB
}

func NewRepository(db *gorm.DB) Repository {
	return &mysqlRepository{db: db}
}

func (r *mysqlRepository) Create(ctx context.Context, p *Product) error {
	return r.db.WithContext(ctx).Create(p).Error
}

func (r *mysqlRepository) GetByID(ctx context.Context, id uint64) (*Product, error) {
	var p Product
	if err := r.db.WithContext(ctx).First(&p, id).Error; err != nil {
		return nil, err
	}
	return &p, nil
}

func (r *mysqlRepository) UpdateFieldsWithVersion(ctx context.Context, id uint64, version int32, fields map[string]any) (int64, error) {
	// 乐观锁核心：版本号既是 WHERE 条件又是更新内容，
	// 「读-改-写」三步并成一条原子 UPDATE，不需要显式加锁。
	fields["version"] = gorm.Expr("version + 1")
	res := r.db.WithContext(ctx).Model(&Product{}).
		Where("id = ? AND version = ?", id, version).
		Updates(fields)
	return res.RowsAffected, res.Error
}

func (r *mysqlRepository) SoftDelete(ctx context.Context, id uint64) (int64, error) {
	res := r.db.WithContext(ctx).Delete(&Product{}, id)
	return res.RowsAffected, res.Error
}

func (r *mysqlRepository) List(ctx context.Context, f ListFilter) ([]*Product, error) {
	q := r.db.WithContext(ctx).Model(&Product{})
	if f.Status != nil {
		q = q.Where("status = ?", *f.Status)
	}
	if f.CategoryID > 0 {
		q = q.Where("category_id = ?", f.CategoryID)
	}
	if f.Keyword != "" {
		// 生产环境需补充：keyword 检索应下沉到搜索引擎（ES/OpenSearch），
		// 前缀通配 LIKE 无法走索引，数据量大时会全表扫描。
		q = q.Where("name LIKE ?", "%"+f.Keyword+"%")
	}
	if f.HasCursor {
		// 游标条件与 ORDER BY 严格同构：(created_at, id) 二元组比较。
		// 建议建联合索引 (status, category_id, created_at, id) 按实际过滤组合调整。
		q = q.Where("(created_at < ? OR (created_at = ? AND id < ?))",
			f.CursorCreatedAt, f.CursorCreatedAt, f.CursorID)
	}
	var items []*Product
	err := q.Order("created_at DESC, id DESC").Limit(f.Limit + 1).Find(&items).Error
	return items, err
}

func (r *mysqlRepository) DeductStock(ctx context.Context, id uint64, qty int32) (*Product, error) {
	var p Product
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 第一重保护：SELECT ... FOR UPDATE 行锁。
		// 悲观但直观——同一行的并发扣减在 DB 层串行化，
		// 后续读到的 Stock 一定是最新的，「先查再改」不会读旧值。
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			First(&p, id).Error; err != nil {
			return err
		}
		if p.Stock < qty {
			return ErrStockNotEnough
		}
		// 第二重保护：Version 乐观锁 + stock >= qty 双条件 UPDATE。
		// 为什么行锁下还要乐观锁：行锁只保护「本事务内」的读写窗口，
		// 一旦有人忘记加锁（新写的代码路径）或跨事务基于旧快照计算后回写，
		// 行锁无能为力；version 不匹配则 0 行受影响，直接兜底失败。
		// 取舍：高并发下行锁是主防线（串行化保证正确），Version 是兜底
		// （防更新丢失的最后一道闸）；stock >= qty 则让扣减条件本身原子化。
		res := tx.Model(&Product{}).
			Where("id = ? AND version = ? AND stock >= ?", p.ID, p.Version, qty).
			Updates(map[string]any{
				"stock":   gorm.Expr("stock - ?", qty),
				"version": gorm.Expr("version + 1"),
			})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return ErrVersionConflict
		}
		// 同步内存态供调用方使用（缓存回填/响应/事件载荷）。
		p.Stock -= qty
		p.Version++
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &p, nil
}
