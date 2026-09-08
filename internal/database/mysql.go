package database

import (
	"context"
	"errors"
	"time"

	drivermysql "github.com/go-sql-driver/mysql"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Open 建立连接池并检查数据库可达性，不创建数据库或自动迁移。
// 调用方负责从环境读取 DSN，并在结束使用后关闭底层 sql.DB。
func Open(ctx context.Context, dsn string) (*gorm.DB, error) {
	if dsn == "" {
		return nil, errors.New("MySQL DSN is required")
	}
	cfg, err := drivermysql.ParseDSN(dsn)
	if err != nil {
		// 不把包含账号密码的原始 DSN 放入错误信息。
		return nil, errors.New("invalid MySQL DSN")
	}
	if cfg.DBName == "" {
		return nil, errors.New("MySQL database name is required")
	}
	// parseTime 让 DATETIME 能读入 time.Time；统一按 UTC 解释时间。
	cfg.ParseTime = true
	cfg.Loc = time.UTC
	cfg.Timeout = 5 * time.Second
	cfg.ReadTimeout = 5 * time.Second
	cfg.WriteTimeout = 5 * time.Second
	db, err := gorm.Open(mysql.New(mysql.Config{
		DSN: cfg.FormatDSN(), SkipInitializeWithVersion: true,
	}), &gorm.Config{
		DisableAutomaticPing: true,
		Logger:               logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		return nil, errors.New("failed to initialize MySQL connection")
	}
	pool, err := db.DB()
	if err != nil {
		return nil, err
	}
	pool.SetMaxOpenConns(5)
	pool.SetMaxIdleConns(2)
	pool.SetConnMaxLifetime(3 * time.Minute)
	// 创建连接池不等于连接成功，PingContext 才进行实际连通性验证。
	if err := pool.PingContext(ctx); err != nil {
		pool.Close()
		return nil, errors.New("MySQL ping failed; check address, credentials and database")
	}
	return db, nil
}
