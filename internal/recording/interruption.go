package recording

import (
	"audiorecording/internal/model"
	"context"
	"gorm.io/gorm"
	"log"
	"time"
)

// InterruptTasks 必须在启动调度器前执行。仅单实例有效，不是阶段恢复或自动重试。
func (s *Service) InterruptTasks(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var interrupted []model.Task
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("status IN ?", []string{model.TaskTranscribing, model.TaskSummarizing}).Find(&interrupted).Error; err != nil {
			return err
		}
		for _, task := range interrupted {
			if err := tx.Model(&model.Task{}).Where("id = ? AND status = ?", task.ID, task.Status).Updates(map[string]any{
				"status": model.TaskFailed, "failed_stage": task.Status, "error_code": "service_interrupted",
				"error_message": "processing interrupted by service restart", "finished_at": time.Now().UTC(),
			}).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, task := range interrupted {
		log.Printf("task transition recording_id=%d task_id=%d from=%s to=failed code=service_interrupted", task.RecordingID, task.ID, task.Status)
	}
	return nil
}
