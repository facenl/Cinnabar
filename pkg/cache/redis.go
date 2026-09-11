// pkg/cache/redis.go
package cache

import (
	"context"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"
)

// ErrNil 统一「缓存未命中」语义。
// 为什么不直接透出 redis.Nil：业务层不应感知底层是 Redis 还是别的
// 实现，便于单测 mock 与将来替换（如本地缓存）。
var ErrNil = errors.New("cache: key not found")

// Client 是对 go-redis 的极薄封装：只做连接管理 + 语义收敛，
// 不在里面塞业务逻辑（业务 key 的拼装属于 service 层职责）。
type Client struct {
	rdb *redis.Client
}

// New 建立连接并立即 Ping 探活——宁可启动失败，也不要运行期
// 才发现地址/密码错误导致全部请求降级打 DB。
func New(addr, password string, db int) (*Client, error) {
	rdb := redis.NewClient(&redis.Options{
		Addr:         addr,
		Password:     password,
		DB:           db,
		DialTimeout:  3 * time.Second,
		ReadTimeout:  2 * time.Second,
		WriteTimeout: 2 * time.Second,
		PoolSize:     50,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		_ = rdb.Close()
		return nil, err
	}
	return &Client{rdb: rdb}, nil
}

// Get 命中返回 value；未命中返回 ErrNil；其余为真实故障。
func (c *Client) Get(ctx context.Context, key string) (string, error) {
	v, err := c.rdb.Get(ctx, key).Result()
	if errors.Is(err, redis.Nil) {
		return "", ErrNil
	}
	return v, err
}

func (c *Client) Set(ctx context.Context, key, value string, ttl time.Duration) error {
	return c.rdb.Set(ctx, key, value, ttl).Err()
}

// Del 允许一次删多个 key；空参数直接返回，避免给 Redis 发无意义命令。
func (c *Client) Del(ctx context.Context, keys ...string) error {
	if len(keys) == 0 {
		return nil
	}
	return c.rdb.Del(ctx, keys...).Err()
}

// Eval 暴露 Lua 执行能力：限流等需要「读-算-写」原子性的场景必须走脚本，
// 否则并发下令牌桶会被超发。
func (c *Client) Eval(ctx context.Context, script string, keys []string, args ...any) *redis.Cmd {
	return c.rdb.Eval(ctx, script, keys, args...)
}

func (c *Client) Close() error { return c.rdb.Close() }
