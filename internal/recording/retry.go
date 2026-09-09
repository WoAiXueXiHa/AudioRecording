package recording

import (
	"context"
	"errors"
	"log"
	"time"

	"audiorecording/internal/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var ErrConflict = errors.New("task state conflicts with operation")

// Retry 复用任务，从转写重新执行；事务同时清空旧结果和旧执行信息。
func (s *Service) Retry(ctx context.Context, id uint64) (UploadResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var result UploadResult
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 重试、删除和阶段保存都先锁任务，再操作录音，避免反向持锁。
		var task model.Task
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", id).Take(&task).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return err
		}
		if task.Status != model.TaskFailed {
			return ErrConflict
		}
		var rec model.Recording
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", task.RecordingID).Take(&rec).Error; err != nil {
			return err
		}
		// map 的 nil 明确写 SQL NULL，不被 GORM 的 struct 零值忽略规则跳过。
		if err := tx.Model(&rec).Updates(map[string]any{"transcript": nil, "summary": nil, "key_points": nil, "todos": nil}).Error; err != nil {
			return err
		}
		if err := tx.Model(&task).Updates(map[string]any{
			"status": model.TaskPending, "retry_count": gorm.Expr("retry_count + 1"),
			"failed_stage": nil, "error_code": nil, "error_message": nil, "started_at": nil, "finished_at": nil,
		}).Error; err != nil {
			return err
		}
		result = UploadResult{RecordingID: task.RecordingID, TaskID: task.ID, Status: model.TaskPending}
		return nil
	})
	if err != nil {
		return UploadResult{}, err
	}
	log.Printf("task transition recording_id=%d task_id=%d from=failed to=pending reason=manual_retry", result.RecordingID, result.TaskID)
	return result, nil
}
