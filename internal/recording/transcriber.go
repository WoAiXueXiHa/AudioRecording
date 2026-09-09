package recording

import (
	"context"
	"errors"
	"math/rand/v2"
	"time"
)

// Transcriber 隔离外部转写行为，测试可直接注入不等待的实现。
type Transcriber interface {
	Transcribe(context.Context, string) (string, error)
}

type mockTranscriber struct{}

func NewMockTranscriber() Transcriber { return mockTranscriber{} }

func (mockTranscriber) Transcribe(ctx context.Context, _ string) (string, error) {
	// 题目只要求模拟转写；不读取音频内容，也不接入真实 ASR。
	timer := time.NewTimer(time.Duration(5+rand.IntN(11)) * time.Second)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-timer.C:
	}
	if rand.IntN(5) == 0 {
		return "", errors.New("mock transcription failed")
	}
	return "今天讨论了录音转写服务的交付安排。首先完成上传和任务查询，然后接入智能摘要。小李负责验证接口，小王负责整理运行说明，计划周五前完成验收。", nil
}
