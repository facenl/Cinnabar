// cmd/server/main.go
package main

import (
	"net"
	"os"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"cinnabar/internal/middleware"
	"cinnabar/internal/product"
	"cinnabar/pkg/cache"
	"cinnabar/pkg/db"
)

// getenv 配置读取的最小实现。生产环境需补充：配置中心/Viper 类方案 +
// 启动时校验（缺必填配置直接退出），但原则不变——配置只进 main，不进包全局变量。
func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	// ---------- 日志 ----------
	log, err := zap.NewProduction()
	if err != nil {
		panic("init logger: " + err.Error())
	}
	defer func() { _ = log.Sync() }()

	// ---------- 基础设施（显式组装，无全局变量） ----------
	mysqlDSN := getenv("MYSQL_DSN",
		"root:root@tcp(127.0.0.1:3306)/cinnabar?charset=utf8mb4&parseTime=True&loc=Local")
	if hp := dsnHostPort(mysqlDSN); hp != "" {
		waitPort(log, "mysql", hp) // 容器编排同起时避免「依赖未就绪即退出」的竞态
	}
	gdb, err := db.NewMySQL(mysqlDSN)
	if err != nil {
		log.Fatal("init mysql", zap.Error(err))
	}

	redisClient, err := cache.New(getenv("REDIS_ADDR", "127.0.0.1:6379"),
		getenv("REDIS_PASSWORD", ""), 0)
	if err != nil {
		log.Fatal("init redis", zap.Error(err))
	}
	defer func() { _ = redisClient.Close() }()

	// 建表仅用于本地一键跑通；生产环境需补充：独立迁移工具
	// （golang-migrate / atlas），DDL 走评审与版本化，禁止服务启动时隐式改表。
	if err := gdb.AutoMigrate(&product.Product{}); err != nil {
		log.Fatal("auto migrate", zap.Error(err))
	}

	// ---------- 依赖注入链：repo → publisher → service → handler ----------
	repo := product.NewRepository(gdb)
	publisher := product.NewNoopPublisher(log) // 生产替换为 Kafka/NATS/RabbitMQ 实现
	svc := product.NewService(repo, redisClient, publisher, log)
	handler := product.NewHandler(svc, log)

	// ---------- 路由 ----------
	r := gin.New()
	r.Use(gin.Recovery())          // panic 兜底，保证单请求崩溃不拖垮进程
	r.Use(middleware.RequestID())  // 必须最先：后续所有日志/响应都依赖它
	r.Use(middleware.AccessLog(log))

	// 认证配置（生产从配置/密钥管理加载）
	jwtSecret := getenv("JWT_SECRET", "dev-only-secret-do-not-use-in-prod")
	// 演示用渠道表：明文 key -> 渠道 key_id。生产必须落库存哈希。
	apiKeys := map[string]string{
		"demo-channel-secret-key": "channel-demo",
	}

	v1 := r.Group("/api/v1")
	v1.Use(middleware.Auth(jwtSecret, apiKeys))
	// 限流必须挂在 Auth 之后：它依赖身份上下文。
	// 演示值 10 QPS / 突发 20；生产应按身份类型分档（渠道配额通常低于前端）。
	v1.Use(middleware.RateLimit(redisClient, 10, 20, log))

	handler.RegisterRoutes(v1)

	// 健康检查不挂认证：K8s liveness/readiness 探针需要。
	r.GET("/healthz", func(c *gin.Context) { c.JSON(200, gin.H{"status": "ok"}) })

	addr := getenv("HTTP_ADDR", ":8080")
	log.Info("server starting", zap.String("addr", addr))
	if err := r.Run(addr); err != nil {
		log.Fatal("server exited", zap.Error(err))
	}
}

// waitPort 等待依赖端口就绪（例如 docker compose 同起时 MySQL 初始化较慢）。
// 指数退避、上限约 30s：比无脑 sleep 更快，也比立即失败更稳。
func waitPort(log *zap.Logger, name, addr string) {
	backoff := 500 * time.Millisecond
	deadline := time.Now().Add(30 * time.Second)
	for {
		conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
		if err == nil {
			_ = conn.Close()
			log.Info(name+" ready", zap.String("addr", addr))
			return
		}
		if time.Now().After(deadline) {
			log.Warn(name+" not ready after 30s, continue anyway",
				zap.String("addr", addr), zap.Error(err))
			return
		}
		time.Sleep(backoff)
		if backoff < 4*time.Second {
			backoff *= 2
		}
	}
}

// dsnHostPort 从 MySQL DSN 中提取 host:port（形如 user:pass@tcp(127.0.0.1:3306)/db）。
// 提取失败返回空串并跳过等待——不阻塞启动。
func dsnHostPort(dsn string) string {
	start := strings.Index(dsn, "tcp(")
	if start < 0 {
		return ""
	}
	end := strings.Index(dsn[start:], ")")
	if end < 0 {
		return ""
	}
	return dsn[start+4 : start+end]
}
