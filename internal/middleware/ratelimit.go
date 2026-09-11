// internal/middleware/ratelimit.go
package middleware

import (
	"fmt"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"cinnabar/pkg/cache"
	"cinnabar/pkg/errcode"
	"cinnabar/pkg/response"
)

// tokenBucketScript Redis 令牌桶，Lua 保证「读-算-写」原子。
// 令牌数用浮点存储：rate 是「每秒补充速率」，通常非整数累计，
// 用整数会丢精度导致长期欠补。
//
// KEYS[1]=桶 key
// ARGV[1]=rate(每秒补充)  ARGV[2]=burst(桶容量)
// ARGV[3]=now(毫秒)      ARGV[4]=requested(本次消耗, 通常为1)
// ARGV[5]=ttl(秒, 桶闲置回收时间)
// 返回 1=放行 0=拒绝
const tokenBucketScript = `
local key      = KEYS[1]
local rate     = tonumber(ARGV[1])
local burst    = tonumber(ARGV[2])
local now      = tonumber(ARGV[3])
local requested= tonumber(ARGV[4])
local ttl      = tonumber(ARGV[5])

local data = redis.call('HMGET', key, 'tokens', 'ts')
local tokens = tonumber(data[1]) or burst
local lastTs = tonumber(data[2]) or now

local delta = math.max(0, now - lastTs)
tokens = math.min(burst, tokens + delta * rate / 1000)

local allowed = 0
if tokens >= requested then
    tokens = tokens - requested
    allowed = 1
end

redis.call('HMSET', key, 'tokens', tokens, 'ts', now)
redis.call('EXPIRE', key, ttl)
return allowed
`

// RateLimit 按身份维度限流：JWT 用户按 user_id，API Key 按 key_id。
// 为什么按身份而不是按 IP：无头架构下渠道方可能共享出口 IP（NAT），
// 按 IP 会误伤；身份与凭证一一对应，配额责任清晰。
func RateLimit(redisClient *cache.Client, ratePerSec, burst int, log *zap.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		idType, _ := c.Get(CtxIdentityType)
		idID, _ := c.Get(CtxIdentityID)
		identity, ok := idID.(string)
		t, _ := idType.(string)
		if !ok || identity == "" {
			// 中间件顺序错误（Auth 未先执行）属于服务端缺陷，按 500 暴露给日志。
			log.Error("rate limit: identity missing, middleware order wrong?")
			response.Fail(c, errcode.ErrInternal, "identity missing")
			return
		}

		key := fmt.Sprintf("rl:%s:%s", t, identity)
		// TTL 取「桶从空补满所需时间」的 2 倍且至少 60s：
		// 闲置身份的桶自然过期回收，避免 Redis 里堆积海量冷 key。
		ttl := int64(2*burst/maxInt(ratePerSec, 1)) + 60

		allowed, err := redisClient.Eval(c.Request.Context(), tokenBucketScript,
			[]string{key},
			ratePerSec, burst, time.Now().UnixMilli(), 1, ttl,
		).Int()
		if err != nil {
			// 限流故障时放行（fail-open）：限流是保护手段不是业务依赖，
			// Redis 抖动不应导致全站 503。生产环境需补充：
			// 对 fail-open 打点告警，必要时切换 fail-close 兜底本地限流。
			log.Warn("rate limit eval failed, fail-open",
				zap.String("identity", identity), zap.Error(err))
			c.Next()
			return
		}
		if allowed != 1 {
			// Retry-After 让守规矩的客户端可以退避重试，而不是无脑重打。
			c.Header("Retry-After", "1")
			response.Fail(c, errcode.ErrTooManyRequests, "rate limit exceeded, please slow down")
			return
		}
		c.Next()
	}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
