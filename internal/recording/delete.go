package recording

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"time"

	"audiorecording/internal/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Delete 只删除终态录音；文件系统不能参加数据库事务，失败边界必须记录。
func (s *Service) Delete(ctx context.Context, id uint64) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var path string
	var taskID uint64
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var task model.Task
		// 与 Retry 和阶段保存保持 task→recording 的锁顺序。无任务的异常录音也可清理。
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("recording_id = ?", id).Take(&task).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if err == nil && task.Status != model.TaskDone && task.Status != model.TaskFailed {
			return ErrConflict
		}
		var rec model.Recording
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", id).Take(&rec).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return err
		}
		path = rec.StoragePath
		taskID = task.ID
		// 先删除文件；权限等错误时保留两表，调用者可以修复后再试。
		// 如果后面的 SQL/Commit 失败，文件无法回滚；缺失文件视为已清理，允许再次 DELETE。
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove recording file: %w", err)
		}
		if task.ID != 0 {
			if err := tx.Delete(&task).Error; err != nil {
				return err
			}
		}
		return tx.Delete(&rec).Error
	})
	if err != nil {
		if !errors.Is(err, ErrConflict) && !errors.Is(err, ErrNotFound) {
			log.Printf("recording deletion failed recording_id=%d task_id=%d path=%q error=%v", id, taskID, path, err)
		}
		return err
	}
	log.Printf("recording deleted recording_id=%d task_id=%d", id, taskID)
	return nil
}
