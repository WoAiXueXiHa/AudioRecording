package recording

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"audiorecording/internal/model"
	"gorm.io/gorm"
)

const MaxFileBytes int64 = 50 * 1024 * 1024

var (
	ErrInvalidFile   = errors.New("invalid audio file")
	ErrTooLarge      = errors.New("audio file exceeds 50 MiB")
	ErrCommitUnknown = errors.New("upload commit result is unknown")
)

type Service struct {
	db  *gorm.DB
	dir string
}

type UploadResult struct {
	RecordingID uint64 `json:"recording_id"`
	TaskID      uint64 `json:"task_id"`
	Status      string `json:"status"`
}

func NewService(db *gorm.DB, dir string) (*Service, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, errors.New("UPLOAD_DIR cannot be empty")
	}
	absolute, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(absolute, 0700); err != nil {
		return nil, fmt.Errorf("prepare upload directory: %w", err)
	}
	return &Service{db: db, dir: absolute}, nil
}

// Upload 不依赖 Gin，只接收请求 context、展示名称和文件内容。
func (s *Service) Upload(ctx context.Context, name string, src io.Reader) (UploadResult, error) {
	var result UploadResult
	// 原名只作展示，不拼入存储路径；兼容客户端传来的 Windows 分隔符。
	name = filepath.Base(strings.ReplaceAll(name, "\\", "/"))
	ext := strings.ToLower(filepath.Ext(name))
	if !utf8.ValidString(name) || utf8.RuneCountInString(name) > 255 {
		return result, ErrInvalidFile
	}
	switch ext {
	case ".wav", ".mp3", ".m4a", ".aac":
	default:
		return result, ErrInvalidFile
	}
	// CreateTemp 随机命名且独占创建，避免重名覆盖和路径穿越；权限默认为 0600。
	file, err := os.CreateTemp(s.dir, "audio-*"+ext)
	if err != nil {
		return result, fmt.Errorf("create audio file: %w", err)
	}
	keepFile := false
	defer func() {
		file.Close()
		if !keepFile {
			if err := os.Remove(file.Name()); err != nil && !errors.Is(err, os.ErrNotExist) {
				log.Printf("upload cleanup failed path=%q error=%v", file.Name(), err)
			}
		}
	}()
	// 多读一个字节才能区分“恰好达到上限”和“已经超限”。不信任客户端声明的大小。
	size, err := io.Copy(file, io.LimitReader(src, MaxFileBytes+1))
	if err != nil {
		return result, fmt.Errorf("save audio file: %w", err)
	}
	if size > MaxFileBytes {
		return result, ErrTooLarge
	}
	if size == 0 {
		return result, ErrInvalidFile
	}
	if err := file.Close(); err != nil {
		return result, fmt.Errorf("close audio file: %w", err)
	}

	recording := model.Recording{OriginalFilename: name, StoragePath: file.Name(), FileSize: uint64(size)}
	task := model.Task{Status: model.TaskPending}
	// 文件已保存才开始短事务，避免复制大文件时占着数据库连接。
	// 显式 Begin/Commit 让 INSERT 失败和 COMMIT 结果不确定能分别处理。
	tx := s.db.WithContext(ctx).Begin()
	if tx.Error != nil {
		return result, fmt.Errorf("begin upload: %w", tx.Error)
	}
	defer tx.Rollback() // 成功 Commit 后无效；提前返回或 panic 时兜底回滚。
	// 两次写入必须都使用 tx；使用 s.db 会跑到事务外。
	if err := tx.Create(&recording).Error; err != nil {
		return result, fmt.Errorf("insert recording: %w", err)
	}
	task.RecordingID = recording.ID // 第一次 INSERT 回填的主键，不假设两个 ID 相同。
	if err := tx.Create(&task).Error; err != nil {
		return result, fmt.Errorf("insert task: %w", err)
	}
	if err := tx.Commit().Error; err != nil {
		// 连接中断可能发生在数据库提交后。保留文件，供核验，不能一律删除。
		keepFile = true
		log.Printf("upload commit uncertain recording_id=%d task_id=%d path=%q error=%v", recording.ID, task.ID, file.Name(), err)
		return result, ErrCommitUnknown
	}
	keepFile = true
	return UploadResult{RecordingID: recording.ID, TaskID: task.ID, Status: task.Status}, nil
}
