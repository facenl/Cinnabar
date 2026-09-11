// internal/product/service.go
package product

import (
	"context"
	"encoding/json"
	"errors"
	"math/rand"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"
	"golang.org/x/sync/singleflight"
	"gorm.io/gorm"

	"cinnabar/pkg/cache"
	"cinnabar/pkg/errcode"
	"cinnabar/pkg/response"
)

// 缓存常量。key 以业务名为前缀，便于多业务共用一个 Redis 实例时治理。
const (
	cacheKeyPrefix = "product:"
	nullValue      = "null"           // 空值缓存标记：防穿透
	detailTTL      = 5 * time.Minute  // 详情缓存 TTL
	nullTTL        = 60 * time.Second // 空值缓存 TTL（短，避免误判长期存在）
)

// 领域事件 topic。命名规则：<聚合>.<动作>，下游（搜索索引/CDN/渠道同步）按 topic 订阅。
const (
	TopicProductCreated       = "product.created"
	TopicProductUpdated       = "product.updated"
	TopicProductDeleted       = "product.deleted"
	TopicProductStockDeducted = "product.stock.deducted"
)

// EventPublisher 领域事件抽象。service 只面向接口，
// 消息中间件选型（Kafka/NATS/RabbitMQ）对业务代码透明。
type EventPublisher interface {
	Publish(ctx context.Context, topic string, payload any) error
}

// noopPublisher 默认空实现：只打日志不发消息，保证本地/测试环境可跑通。
// 生产环境在 main.go 装配时替换为真实 MQ 实现即可，业务代码零改动。
type noopPublisher struct {
	log *zap.Logger
}

func NewNoopPublisher(log *zap.Logger) EventPublisher {
	return &noopPublisher{log: log}
}

func (n *noopPublisher) Publish(ctx context.Context, topic string, payload any) error {
	n.log.Debug("domain event (noop)",
		zap.String("topic", topic),
		zap.Any("payload", payload),
		zap.String("request_id", response.RequestIDFromContext(ctx)),
	)
	return nil
}

// BizError 业务错误：携带错误码，由 handler 统一翻译为响应体。
// 用类型化错误而不是直接返回 errcode 常量，是为了能携带场景化文案。
type BizError struct {
	Code int
	Msg  string
}

func (e *BizError) Error() string { return e.Msg }

func newBizError(code int, msg string) *BizError {
	if msg == "" {
		msg = errcode.Message(code)
	}
	return &BizError{Code: code, Msg: msg}
}

func errProductNotFound() *BizError {
	return newBizError(errcode.ErrProductNotFound, "product not found")
}

// ListResult service 层分页结果（DTO 由 handler 组装）。
type ListResult struct {
	Items      []*Product
	NextCursor string
	HasMore    bool
}

// Service 商品业务服务。依赖全部构造函数注入：无全局变量，
// 单测时可以用 fake repo / miniredis / noop publisher 自由组合。
type Service struct {
	repo   Repository
	cache  *cache.Client
	pub    EventPublisher
	log    *zap.Logger
	sfGroup singleflight.Group
}

func NewService(repo Repository, cacheClient *cache.Client, pub EventPublisher, log *zap.Logger) *Service {
	return &Service{repo: repo, cache: cacheClient, pub: pub, log: log}
}

func cacheKey(id uint64) string {
	return cacheKeyPrefix + strconv.FormatUint(id, 10)
}

// jitterTTL 缓存雪崩防护：TTL 加 0~20% 随机抖动。
// 若大批 key 在同一时刻写入（如启动预热、批量导入），固定 TTL 会在
// 同一时刻集体过期引发回源风暴；抖动把过期时间点摊开。
func jitterTTL(base time.Duration) time.Duration {
	return base + time.Duration(rand.Int63n(int64(base)/5))
}

func (s *Service) logger(ctx context.Context) *zap.Logger {
	return s.log.With(zap.String("request_id", response.RequestIDFromContext(ctx)))
}

// evictCache Cache Aside 模式的「写后失效」。
// 删除失败不阻塞主流程（DB 已是真相，缓存最多脏一个 TTL 周期），
// 只记日志。生产环境需补充：失败重试队列，或基于 binlog（Canal/Debezium）
// 的缓存同步，彻底消除「更新 DB 成功但删缓存失败」的窗口。
func (s *Service) evictCache(ctx context.Context, id uint64) {
	if err := s.cache.Del(ctx, cacheKey(id)); err != nil {
		s.logger(ctx).Warn("cache eviction failed",
			zap.Uint64("product_id", id), zap.Error(err))
	}
}

// publish 事件发布失败同样不阻塞主流程。
// 生产环境需补充：Transactional Outbox（同事务落库事件表 + 异步 relay），
// 否则「DB 提交成功、消息发送失败」会导致下游索引/渠道数据永久滞后。
func (s *Service) publish(ctx context.Context, topic string, payload any) {
	if err := s.pub.Publish(ctx, topic, payload); err != nil {
		s.logger(ctx).Warn("publish domain event failed",
			zap.String("topic", topic), zap.Error(err))
	}
}

// Create 创建商品。唯一索引冲突（重复商品名）翻译为参数级错误。
func (s *Service) Create(ctx context.Context, req *ProductCreateReq) (*Product, error) {
	status := StatusOn
	if req.Status != nil {
		status = *req.Status
	}
	p := &Product{
		Name:        req.Name,
		Description: req.Description,
		Price:       req.Price,
		Stock:       req.Stock,
		CategoryID:  req.CategoryID,
		Status:      status,
	}
	if err := s.repo.Create(ctx, p); err != nil {
		// MySQL 1062 唯一键冲突。不引入 mysql driver 错误包做类型断言，
		// 用子串判断保持仓储层技术栈解耦（够用但脆弱，生产可用 errors.As + MySQLError）。
		if isDuplicateErr(err) {
			return nil, newBizError(errcode.ErrInvalidParam, "product name already exists")
		}
		return nil, err
	}
	s.publish(ctx, TopicProductCreated, map[string]any{"product_id": p.ID})
	return p, nil
}

// GetByID 详情读取，覆盖缓存三条路径：
//  1. 命中真实缓存 → 直接返回
//  2. 命中空值缓存（"null" 标记）→ 直接 404，挡住对不存在 id 的恶意刷取（防穿透）
//  3. 未命中 → singleflight 合并并发回源（防击穿）→ 查库 → 回填（含空值回填）
func (s *Service) GetByID(ctx context.Context, id uint64) (*Product, error) {
	key := cacheKey(id)

	val, err := s.cache.Get(ctx, key)
	switch {
	case err == nil:
		if val == nullValue {
			return nil, errProductNotFound()
		}
		var p Product
		if json.Unmarshal([]byte(val), &p) == nil {
			return &p, nil
		}
		// 反序列化失败说明缓存数据损坏：不致命，当作未命中回源并覆盖。
	case errors.Is(err, cache.ErrNil):
		// 正常未命中，继续回源
	default:
		// Redis 故障降级：缓存是加速层不是依赖项，直接回源 DB 保可用性。
		s.logger(ctx).Warn("cache get failed, fallback to db",
			zap.Uint64("product_id", id), zap.Error(err))
	}

	// singleflight 防击穿：热点 key 过期瞬间的 N 个并发请求只回源一次。
	// 可选优化，在 QPS 不高的场景可移除；但在「秒杀详情页」这类
	// 单 key 热点场景，它把回源压力从 N 降到 1，成本极低。
	v, err, _ := s.sfGroup.Do(key, func() (any, error) {
		p, derr := s.repo.GetByID(ctx, id)
		if errors.Is(derr, gorm.ErrRecordNotFound) {
			// 空值缓存：DB 确认不存在后写入 "null" 标记，
			// 后续请求在路径 2 直接短路，DB 完全无感。
			if serr := s.cache.Set(ctx, key, nullValue, jitterTTL(nullTTL)); serr != nil {
				s.logger(ctx).Warn("set null cache failed", zap.Error(serr))
			}
			return nil, errProductNotFound()
		}
		if derr != nil {
			return nil, derr
		}
		if b, jerr := json.Marshal(p); jerr == nil {
			if serr := s.cache.Set(ctx, key, string(b), jitterTTL(detailTTL)); serr != nil {
				s.logger(ctx).Warn("backfill cache failed", zap.Error(serr))
			}
		}
		return p, nil
	})
	if err != nil {
		return nil, err
	}
	return v.(*Product), nil
}

// Update 乐观锁更新：先更新 DB，再失效缓存（Cache Aside）。
func (s *Service) Update(ctx context.Context, id uint64, req *ProductUpdateReq) (*Product, error) {
	fields := make(map[string]any)
	if req.Name != nil {
		fields["name"] = *req.Name
	}
	if req.Description != nil {
		fields["description"] = *req.Description
	}
	if req.Price != nil {
		fields["price"] = *req.Price
	}
	if req.Stock != nil {
		fields["stock"] = *req.Stock
	}
	if req.CategoryID != nil {
		fields["category_id"] = *req.CategoryID
	}
	if req.Status != nil {
		fields["status"] = *req.Status
	}
	if len(fields) == 0 {
		return nil, newBizError(errcode.ErrInvalidParam, "no fields to update")
	}

	affected, err := s.repo.UpdateFieldsWithVersion(ctx, id, req.Version, fields)
	if err != nil {
		if isDuplicateErr(err) {
			return nil, newBizError(errcode.ErrInvalidParam, "product name already exists")
		}
		return nil, err
	}
	if affected == 0 {
		// 0 行受影响有两种可能：记录不存在，或版本不匹配。
		// 需要回查一次以返回准确的 404 / 409，这是乐观锁的固有成本。
		if _, gerr := s.repo.GetByID(ctx, id); gerr != nil {
			return nil, errProductNotFound()
		}
		return nil, newBizError(errcode.ErrProductUpdateConflict,
			"product has been modified by others, please refresh and retry")
	}

	// 回读最新态返回给前端（含自增后的 version），并失效缓存。
	p, err := s.repo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	s.evictCache(ctx, id)
	s.publish(ctx, TopicProductUpdated, map[string]any{"product_id": id})
	return p, nil
}

// Delete 软删除 + 缓存失效。
func (s *Service) Delete(ctx context.Context, id uint64) error {
	affected, err := s.repo.SoftDelete(ctx, id)
	if err != nil {
		return err
	}
	if affected == 0 {
		return errProductNotFound()
	}
	s.evictCache(ctx, id)
	s.publish(ctx, TopicProductDeleted, map[string]any{"product_id": id})
	return nil
}

// List 游标分页查询。
func (s *Service) List(ctx context.Context, q *ProductListQuery) (*ListResult, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = defaultLimit
	}
	if limit > maxLimit {
		limit = maxLimit
	}
	f := ListFilter{
		Status:     q.Status,
		CategoryID: q.CategoryID,
		Keyword:    q.Keyword,
		Limit:      limit,
	}
	if q.Cursor != "" {
		t, cursorID, err := DecodeCursor(q.Cursor)
		if err != nil {
			return nil, newBizError(errcode.ErrInvalidParam, "invalid cursor")
		}
		f.HasCursor = true
		f.CursorCreatedAt = t
		f.CursorID = cursorID
	}

	items, err := s.repo.List(ctx, f)
	if err != nil {
		return nil, err
	}
	// 多取的 1 条只用于判断 has_more，不返回给客户端。
	hasMore := len(items) > limit
	if hasMore {
		items = items[:limit]
	}
	next := ""
	if hasMore && len(items) > 0 {
		last := items[len(items)-1]
		next = EncodeCursor(last.CreatedAt, last.ID)
	}
	return &ListResult{Items: items, NextCursor: next, HasMore: hasMore}, nil
}

// DeductStock 扣减库存：事务 + 行锁 + 乐观锁（见 repository 实现注释），
// 成功后失效缓存并发布领域事件（下游可同步渠道库存）。
func (s *Service) DeductStock(ctx context.Context, id uint64, qty int32) (*Product, error) {
	p, err := s.repo.DeductStock(ctx, id, qty)
	if err != nil {
		switch {
		case errors.Is(err, gorm.ErrRecordNotFound):
			return nil, errProductNotFound()
		case errors.Is(err, ErrStockNotEnough):
			return nil, newBizError(errcode.ErrProductStockNotEnough, "product stock not enough")
		case errors.Is(err, ErrVersionConflict):
			return nil, newBizError(errcode.ErrProductUpdateConflict, "stock deduct conflict, please retry")
		default:
			return nil, err
		}
	}
	s.evictCache(ctx, id)
	s.publish(ctx, TopicProductStockDeducted, map[string]any{
		"product_id":      id,
		"deduct_quantity": qty,
		"remaining_stock": p.Stock,
	})
	return p, nil
}

// isDuplicateErr 判断 MySQL 唯一键冲突（errno 1062 的文本特征）。
// 用子串判断保持仓储层与具体 driver 解耦（够用但脆弱；
// 生产环境可开启 gorm TranslateError 后用 errors.Is(err, gorm.ErrDuplicatedKey)）。
func isDuplicateErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "Duplicate entry")
}
