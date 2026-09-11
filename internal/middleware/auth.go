// internal/middleware/auth.go
package middleware

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"strings"

	"github.com/gin-gonic/gin"

	"cinnabar/pkg/errcode"
	"cinnabar/pkg/response"
)

// gin.Context 中身份信息的约定键，下游 handler / 限流中间件从这里取。
const (
	CtxIdentityType = "identity_type"
	CtxIdentityID   = "identity_id"

	IdentityTypeUser   = "user"    // JWT 认证的自研前端用户
	IdentityTypeAPIKey = "api_key" // API Key 认证的第三方渠道
)

// jwtClaims 是本服务关心的 JWT 载荷子集。
// 标准声明用别名兼容（"sub"/"exp"），也接受 "user_id" 直出。
type jwtClaims struct {
	Subject  string `json:"sub"`
	UserID   string `json:"user_id"`
	ExpireAt int64  `json:"exp"`
}

func (c *jwtClaims) uid() string {
	if c.UserID != "" {
		return c.UserID
	}
	return c.Subject
}

// Auth 双模式认证中间件：先 JWT（Authorization: Bearer），失败后 API Key（X-API-Key）。
// 为什么 JWT 优先：它是自研前端的主路径，占比最高；且 JWT 自带过期，
// 安全性上优先验证更严格的凭证。
//
// ⚠️ 生产环境需补充：
//   - jwtSecret 应来自密钥管理服务/环境变量，禁止硬编码进仓库；
//   - apiKeys 应落库（存哈希而非明文）并支持吊销、过期、渠道 scope；
//     本项目为非对称加密/数据库查询预留了接口位置（verifyJWT / lookupAPIKey）。
func Auth(jwtSecret string, apiKeys map[string]string) gin.HandlerFunc {
	return func(c *gin.Context) {
		// 路径一：JWT
		authHeader := c.GetHeader("Authorization")
		if strings.HasPrefix(authHeader, "Bearer ") {
			token := strings.TrimSpace(strings.TrimPrefix(authHeader, "Bearer "))
			if uid, ok := verifyJWT(token, jwtSecret); ok {
				c.Set(CtxIdentityType, IdentityTypeUser)
				c.Set(CtxIdentityID, uid)
				c.Next()
				return
			}
			// JWT 存在但无效：仍然允许降级尝试 API Key，
			// 便于前端 token 过期与渠道调用并存的排查场景。
		}

		// 路径二：API Key
		if key := strings.TrimSpace(c.GetHeader("X-API-Key")); key != "" {
			if keyID, ok := lookupAPIKey(key, apiKeys); ok {
				c.Set(CtxIdentityType, IdentityTypeAPIKey)
				c.Set(CtxIdentityID, keyID)
				c.Next()
				return
			}
		}

		// 两种模式都失败：统一 401，不区分「没有凭证」和「凭证错误」，
		// 避免给攻击者枚举有效凭证的线索。
		response.Fail(c, errcode.ErrUnauthorized, "invalid or missing credentials")
	}
}

// verifyJWT 校验 HS256 JWT 的签名与 exp，返回用户 ID。
// 只接受 HS256（alg 白名单）——这是防 alg 混淆攻击（none / RS256→HS256）的关键。
// 生产环境需补充：改用标准 JWT 库或对接 SSO 的 JWKS 公钥（RS256/ES256），
// 并校验 iss / aud / nbf。
func verifyJWT(token, secret string) (string, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", false
	}
	var header struct {
		Alg string `json:"alg"`
	}
	headerBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil || json.Unmarshal(headerBytes, &header) != nil || header.Alg != "HS256" {
		return "", false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(parts[0] + "." + parts[1]))
	expected := mac.Sum(nil)
	actual, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || !hmac.Equal(actual, expected) {
		return "", false
	}
	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", false
	}
	var claims jwtClaims
	if err := json.Unmarshal(payloadBytes, &claims); err != nil {
		return "", false
	}
	// exp 存在则必须未过期；空 exp 视为不过期（内部服务间短 token 场景）。
	if claims.ExpireAt != 0 && claims.ExpireAt < nowUnix() {
		return "", false
	}
	uid := claims.uid()
	if uid == "" {
		return "", false
	}
	return uid, true
}

// lookupAPIKey 查渠道凭证表，返回渠道 key_id。
// 生产环境需补充：改为数据库查询（明文 key 存 SHA-256 哈希），
// 并携带 rate limit 档位、scope、过期时间等渠道元数据。
func lookupAPIKey(key string, apiKeys map[string]string) (string, bool) {
	for raw, id := range apiKeys {
		if hmac.Equal([]byte(raw), []byte(key)) { // 恒定时间比较，防时序侧信道
			return id, true
		}
	}
	return "", false
}
