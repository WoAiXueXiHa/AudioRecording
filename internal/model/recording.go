package model

import "time"

type Recording struct {
	ID               uint64 `gorm:"primaryKey;autoIncrement"`
	OriginalFilename string
	StoragePath      string
	FileSize         uint64
	// 指针用来区分 SQL NULL（尚未生成）和空字符串（已保存的空文本）。
	Transcript *string
	Summary    *string
	// serializer:json 负责 []string 与 JSON 之间的转换。
	// nil 切片写为 SQL NULL；非 nil 的空切片 []string{} 写为 JSON []。
	KeyPoints []string `gorm:"serializer:json;type:json"`
	Todos     []string `gorm:"serializer:json;type:json"`
	CreatedAt time.Time
	UpdatedAt time.Time
}
