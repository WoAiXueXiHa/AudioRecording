package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"audiorecording/internal/model"

	drivermysql "github.com/go-sql-driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestMySQLMapping(t *testing.T) {
	dsn := os.Getenv("TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("set TEST_MYSQL_DSN for a disposable MySQL instance with CREATE DATABASE permission")
	}
	cfg, err := drivermysql.ParseDSN(dsn)
	if err != nil {
		t.Fatal("invalid TEST_MYSQL_DSN")
	}
	// 不使用 DSN 中的业务库：只操作本次创建的随机命名测试库。
	cfg.DBName = ""
	cfg.Timeout = 5 * time.Second
	admin, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		t.Fatal("cannot open test MySQL connection")
	}
	defer admin.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	name := fmt.Sprintf("audio_mapping_%d_test", time.Now().UnixNano())
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE `"+name+"` CHARACTER SET utf8mb4"); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanupCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if _, err := admin.ExecContext(cleanupCtx, "DROP DATABASE `"+name+"`"); err != nil {
			t.Errorf("test database cleanup failed: %v", err)
		}
	}()
	cfg.DBName = name
	db, err := Open(ctx, cfg.FormatDSN())
	if err != nil {
		t.Fatal(err)
	}
	pool, _ := db.DB()
	defer pool.Close()
	// 只在测试中输出 SQL；本测试数据均为固定的非敏感样例。
	db = db.WithContext(ctx).Session(&gorm.Session{Logger: logger.Default.LogMode(logger.Info)})
	migration, err := os.ReadFile("../../migrations/001_init.sql")
	if err != nil {
		t.Fatal(err)
	}
	// 当前迁移只有两个 CREATE TABLE，没有存储过程或含分号的字符串。
	for _, statement := range strings.Split(string(migration), ";") {
		if strings.TrimSpace(statement) != "" {
			if err := db.Exec(statement).Error; err != nil {
				t.Fatal(err)
			}
		}
	}
	t.Run("queries", func(t *testing.T) { testQueries(t, db) })
	t.Run("runner", func(t *testing.T) { testRunner(t, db) })
	t.Run("summary_runner", func(t *testing.T) { testSummaryRunner(t, db) })
	t.Run("retry", func(t *testing.T) { testRetry(t, db) })
	t.Run("delete", func(t *testing.T) { testDelete(t, db) })
	recording := model.Recording{OriginalFilename: "sample.wav", StoragePath: "test/sample.wav", FileSize: 42}
	if err := db.Create(&recording).Error; err != nil {
		t.Fatal(err)
	}
	var got model.Recording
	if err := db.First(&got, recording.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.ID == 0 || got.OriginalFilename != recording.OriginalFilename || got.CreatedAt.IsZero() || got.KeyPoints != nil || got.Transcript != nil {
		t.Fatalf("unexpected initial mapping: %+v", got)
	}
	var nullCount int64
	if err := db.Model(&model.Recording{}).Where("id = ? AND key_points IS NULL AND todos IS NULL", recording.ID).Count(&nullCount).Error; err != nil {
		t.Fatal(err)
	}
	if nullCount != 1 {
		t.Fatal("nil slices must be SQL NULL")
	}
	recording.KeyPoints = []string{"使用 Gin", "保存 MySQL"}
	recording.Todos = []string{}
	// 用 struct 保留字段 serializer；Select 明确本次要更新的字段。
	if err := db.Model(&recording).Select("KeyPoints", "Todos").Updates(&recording).Error; err != nil {
		t.Fatal(err)
	}
	got = model.Recording{}
	if err := db.First(&got, recording.ID).Error; err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.KeyPoints, recording.KeyPoints) || got.Todos == nil || len(got.Todos) != 0 {
		t.Fatalf("JSON round trip failed: %+v", got)
	}
	task := model.Task{RecordingID: recording.ID}
	if err := db.Create(&task).Error; err != nil {
		t.Fatal(err)
	}
	var saved model.Task
	if err := db.First(&saved, task.ID).Error; err != nil || saved.Status != model.TaskPending || saved.RecordingID != recording.ID {
		t.Fatalf("task mapping failed: %+v, %v", saved, err)
	}
	expectMySQLError(t, db.Create(&model.Task{RecordingID: recording.ID}).Error, 1062)
	expectMySQLError(t, db.Create(&model.Task{RecordingID: recording.ID + 100}).Error, 1452)
	expectMySQLError(t, db.Delete(&model.Recording{}, recording.ID).Error, 1451)
	t.Run("upload", func(t *testing.T) { testUpload(t, db) })
}

func expectMySQLError(t *testing.T, err error, number uint16) {
	t.Helper()
	var mysqlErr *drivermysql.MySQLError
	if !errors.As(err, &mysqlErr) || mysqlErr.Number != number {
		t.Fatalf("expected MySQL error %d, got %v", number, err)
	}
}
