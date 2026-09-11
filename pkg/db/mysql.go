// pkg/db/mysql.go
package db

import (
	"time"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// NewMySQL 初始化 GORM 客户端。
// 连接池参数在此统一收口：如果由各业务方自行调参，
// 多个模块叠加很容易把 MySQL max_connections 打满。
func NewMySQL(dsn string) (*gorm.DB, error) {
	gdb, err := gorm.Open(mysql.Open(dsn), &gorm.Config{
		// 只记 Warn 及以上：生产环境 SQL 全量日志量太大。
		// 生产环境需补充：实现 gorm logger.Interface 桥接到 zap，
		// 保持全服务单一日志管道，并携带 request_id。
		Logger: gormlogger.Default.LogMode(gormlogger.Warn),
	})
	if err != nil {
		return nil, err
	}

	sqlDB, err := gdb.DB()
	if err != nil {
		return nil, err
	}
	sqlDB.SetMaxOpenConns(50)
	sqlDB.SetMaxIdleConns(10)
	sqlDB.SetConnMaxLifetime(time.Hour) // 小于 MySQL wait_timeout，避免拿到被服务端断掉的连接

	// 启动即探活：fail fast。
	if err := sqlDB.Ping(); err != nil {
		return nil, err
	}
	return gdb, nil
}
