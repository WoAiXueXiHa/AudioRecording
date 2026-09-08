CREATE TABLE recordings (
    id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    original_filename VARCHAR(255) NOT NULL,            -- 原始文件名
    storage_path VARCHAR(1024) NOT NULL,                -- 文件系统保存路径
    file_size BIGINT UNSIGNED NOT NULL,
    transcript LONGTEXT NULL,                           -- 转换结果
    summary TEXT NULL,                                  -- 一句话摘要
    key_points JSON NULL,                               -- 解析的关键要点
    todos JSON NULL,                                    -- 待办要点
    created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (id),
    -- created_at 相同的记录由 id 决定先后顺序
    INDEX idx_recordings_created_id (created_at DESC, id DESC)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE tasks (
    id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    recording_id BIGINT UNSIGNED NOT NULL,
    status VARCHAR(20) NOT NULL DEFAULT 'pending',
    retry_count INT UNSIGNED NOT NULL DEFAULT 0,
    failed_stage VARCHAR(20) NULL,
    error_code VARCHAR(64) NULL,
    error_message TEXT NULL,
    started_at DATETIME(3) NULL,
    finished_at DATETIME(3) NULL,
    created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (id),
    -- 唯一约束：同一录音最多一个任务，重试复用该任务
    CONSTRAINT uq_tasks_recording UNIQUE (recording_id),
    -- 外键：任务不能引用不存在的录音；删除时先删任务，再删录音
    CONSTRAINT fk_tasks_recording FOREIGN KEY (recording_id)
        REFERENCES recordings (id) ON DELETE RESTRICT,
    CONSTRAINT chk_tasks_status CHECK (
        status IN ('pending', 'transcribing', 'summarizing', 'done', 'failed')
    ),
    -- 后台按状态筛选，再按创建时间和 id 领取任务
    INDEX idx_tasks_status_created_id (status, created_at, id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
