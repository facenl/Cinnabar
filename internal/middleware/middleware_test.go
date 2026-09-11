// internal/middleware/middleware_test.go
package middleware_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"cinnabar/internal/middleware"
	"cinnabar/pkg/cache"
	"cinnabar/pkg/errcode"
	"cinnabar/pkg/response"
)

const testSecret = "test-secret"

// makeJWT 手工签发 HS256 token，与服务端 verifyJWT 的手写校验互为镜像。
func makeJWT(sub string, exp int64) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	payload := base64.RawURLEncoding.EncodeToString(
		[]byte(fmt.Sprintf(`{"sub":%q,"exp":%d}`, sub, exp)))
	mac := hmac.New(sha256.New, []byte(testSecret))
	mac.Write([]byte(header + "." + payload))
	return header + "." + payload + "." +
		base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func setupRouter(t *testing.T) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(middleware.RequestID())
	g := r.Group("/api/v1")
	g.Use(middleware.Auth(testSecret, map[string]string{"good-key": "channel-1"}))
	g.GET("/ping", func(c *gin.Context) {
		t2, _ := c.Get(middleware.CtxIdentityType)
		id, _ := c.Get(middleware.CtxIdentityID)
		response.Success(c, gin.H{"identity_type": t2, "identity_id": id})
	})
	return r
}

type respBody struct {
	Data   json.RawMessage `json:"data"`
	Errors []struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"errors"`
	Meta struct {
		RequestID string `json:"request_id"`
		Version   string `json:"version"`
		Timestamp int64  `json:"timestamp"`
	} `json:"meta"`
}

func doRequest(t *testing.T, r *gin.Engine, headers map[string]string) (int, respBody) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/ping", nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	var body respBody
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("response not json: %v, body=%s", err, w.Body.String())
	}
	return w.Code, body
}

func TestUnifiedResponseHasMeta(t *testing.T) {
	r := setupRouter(t)
	code, body := doRequest(t, r, map[string]string{"X-API-Key": "good-key"})
	if code != http.StatusOK {
		t.Fatalf("want 200, got %d", code)
	}
	if body.Meta.RequestID == "" || body.Meta.Version != "v1" || body.Meta.Timestamp == 0 {
		t.Fatalf("meta incomplete: %+v", body.Meta)
	}
}

func TestAuthAPIKey(t *testing.T) {
	r := setupRouter(t)
	code, body := doRequest(t, r, map[string]string{"X-API-Key": "good-key"})
	if code != http.StatusOK {
		t.Fatalf("want 200, got %d", code)
	}
	var data struct {
		Type string `json:"identity_type"`
		ID   string `json:"identity_id"`
	}
	if err := json.Unmarshal(body.Data, &data); err != nil {
		t.Fatal(err)
	}
	if data.Type != middleware.IdentityTypeAPIKey || data.ID != "channel-1" {
		t.Fatalf("wrong identity: %+v", data)
	}
}

func TestAuthJWT(t *testing.T) {
	r := setupRouter(t)
	token := makeJWT("user-42", time.Now().Add(time.Hour).Unix())
	code, body := doRequest(t, r, map[string]string{"Authorization": "Bearer " + token})
	if code != http.StatusOK {
		t.Fatalf("want 200, got %d", code)
	}
	var data struct {
		Type string `json:"identity_type"`
		ID   string `json:"identity_id"`
	}
	if err := json.Unmarshal(body.Data, &data); err != nil {
		t.Fatal(err)
	}
	if data.Type != middleware.IdentityTypeUser || data.ID != "user-42" {
		t.Fatalf("wrong identity: %+v", data)
	}
}

func TestAuthRejectExpiredJWTAndNoCredential(t *testing.T) {
	r := setupRouter(t)
	expired := makeJWT("user-42", time.Now().Add(-time.Hour).Unix())
	code, body := doRequest(t, r, map[string]string{"Authorization": "Bearer " + expired})
	if code != http.StatusUnauthorized || len(body.Errors) == 0 ||
		body.Errors[0].Code != errcode.ErrUnauthorized {
		t.Fatalf("want 401/%d, got %d/%+v", errcode.ErrUnauthorized, code, body)
	}
	if body.Meta.RequestID == "" {
		t.Fatal("unauthorized response must still carry request_id")
	}

	code, _ = doRequest(t, r, nil)
	if code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", code)
	}
}

func TestAuthRejectBadSignature(t *testing.T) {
	r := setupRouter(t)
	token := makeJWT("user-42", time.Now().Add(time.Hour).Unix()) + "tampered"
	code, _ := doRequest(t, r, map[string]string{"Authorization": "Bearer " + token})
	if code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", code)
	}
}

// TestRateLimit 依赖本机 Redis（127.0.0.1:6379），不可达时跳过而非失败。
func TestRateLimit(t *testing.T) {
	cli, err := cache.New("127.0.0.1:6379", "", 1)
	if err != nil {
		t.Skipf("redis not available: %v", err)
	}
	defer cli.Close()

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(middleware.RequestID())
	log := zap.NewNop()
	g := r.Group("/api/v1")
	g.Use(middleware.Auth(testSecret, map[string]string{"rl-key": "rl-test-" + fmt.Sprint(time.Now().UnixNano())}))
	g.Use(middleware.RateLimit(cli, 1, 2, log)) // 1 QPS / 突发 2
	g.GET("/ping", func(c *gin.Context) { response.Success(c, gin.H{"ok": true}) })

	hit := func() int {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/ping", nil)
		req.Header.Set("X-API-Key", "rl-key")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code
	}
	if c := hit(); c != http.StatusOK {
		t.Fatalf("1st request want 200, got %d", c)
	}
	if c := hit(); c != http.StatusOK {
		t.Fatalf("2nd request (within burst) want 200, got %d", c)
	}
	if c := hit(); c != http.StatusTooManyRequests {
		t.Fatalf("3rd request want 429, got %d", c)
	}
}
