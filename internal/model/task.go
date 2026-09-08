package model

import "time"

const (
	TaskPending      = "pending"
	TaskTranscribing = "transcribing"
	TaskSummarizing  = "summarizing"
	TaskDone         = "done"
	TaskFailed       = "failed"
)

// Task 只保存本轮执行信息，不保存每次重试的历史
// RecordingID 是关联字段；唯一性与外键由 SQL 约束真正保证
type Task struct {
	ID           uint64 `gorm:"primaryKey;autoIncrement"`
	RecordingID  uint64
	Status       string `gorm:"default:pending"`
	RetryCount   uint32
	FailedStage  *string
	ErrorCode    *string
	ErrorMessage *string
	StartedAt    *time.Time
	FinishedAt   *time.Time
	CreatedAt    time.Time
	UpdatedAt    time.Time
}
