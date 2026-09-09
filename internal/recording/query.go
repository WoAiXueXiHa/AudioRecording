package recording

import (
	"context"
	"errors"
	"time"

	"audiorecording/internal/model"
	"gorm.io/gorm"
)

var ErrNotFound = errors.New("resource not found")

// 响应结构独立于数据库 model，避免把服务器 storage_path 暴露给客户端。
type RecordingItem struct {
	ID               uint64    `json:"id"`
	OriginalFilename string    `json:"original_filename"`
	FileSize         uint64    `json:"file_size"`
	CreatedAt        time.Time `json:"created_at"`
	// LEFT JOIN 下缺少任务时返回 null，避免把录音悄悄从列表中隐藏。
	TaskID *uint64 `json:"task_id"`
	Status *string `json:"status"`
}

type RecordingDetail struct {
	RecordingItem `gorm:"embedded"`
	Transcript    *string   `json:"transcript"`
	Summary       *string   `json:"summary"`
	KeyPoints     []string  `json:"key_points" gorm:"serializer:json"`
	Todos         []string  `json:"todos" gorm:"serializer:json"`
	UpdatedAt     time.Time `json:"updated_at"`
}

type RecordingPage struct {
	Items    []RecordingItem `json:"items"`
	Total    int64           `json:"total"`
	Page     int             `json:"page"`
	PageSize int             `json:"page_size"`
}

type TaskDetail struct {
	ID           uint64     `json:"id"`
	RecordingID  uint64     `json:"recording_id"`
	Status       string     `json:"status"`
	RetryCount   uint32     `json:"retry_count"`
	FailedStage  *string    `json:"failed_stage"`
	ErrorCode    *string    `json:"error_code"`
	ErrorMessage *string    `json:"error_message"`
	StartedAt    *time.Time `json:"started_at"`
	FinishedAt   *time.Time `json:"finished_at"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
}

func (s *Service) Task(ctx context.Context, id uint64) (TaskDetail, error) {
	var task TaskDetail
	// Take 查询单条；不存在时返回 ErrRecordNotFound。参数占位符不拼接用户输入。
	err := s.db.WithContext(ctx).Model(&model.Task{}).Where("id = ?", id).Take(&task).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return task, ErrNotFound
	}
	return task, err
}

const itemColumns = "r.id, r.original_filename, r.file_size, r.created_at, t.id AS task_id, t.status"

func (s *Service) Detail(ctx context.Context, id uint64) (RecordingDetail, error) {
	var detail RecordingDetail
	err := s.db.WithContext(ctx).Table("recordings AS r").
		Select(itemColumns+", r.transcript, r.summary, r.key_points, r.todos, r.updated_at").
		Joins("LEFT JOIN tasks AS t ON t.recording_id = r.id").
		Where("r.id = ?", id).Take(&detail).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return detail, ErrNotFound
	}
	return detail, err
}

func (s *Service) List(ctx context.Context, page, pageSize int) (RecordingPage, error) {
	result := RecordingPage{Items: make([]RecordingItem, 0), Page: page, PageSize: pageSize}
	// COUNT + 一次 JOIN，共两条 SELECT，与这一页的条目数无关。
	if err := s.db.WithContext(ctx).Model(&model.Recording{}).Count(&result.Total).Error; err != nil {
		return result, err
	}
	err := s.db.WithContext(ctx).Table("recordings AS r").Select(itemColumns).
		Joins("LEFT JOIN tasks AS t ON t.recording_id = r.id").
		Order("r.created_at DESC, r.id DESC").Limit(pageSize).Offset((page - 1) * pageSize).
		Find(&result.Items).Error
	// Find 没有匹配行是空切片，不是 ErrRecordNotFound。唯一关联约束防止 JOIN 扩大行数。
	return result, err
}
