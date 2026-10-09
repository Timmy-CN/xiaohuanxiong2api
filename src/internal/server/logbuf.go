package server

import (
	"sync"
	"time"
)

// RequestLog 是一条请求日志（SPEC §6.4 的字段）。
type RequestLog struct {
	Time             string `json:"time"`
	Method           string `json:"method"`
	Path             string `json:"path"`
	Model            string `json:"model"`
	ClientModel      string `json:"client_model"`
	Account          string `json:"account"`
	Status           int    `json:"status"`
	Stream           bool   `json:"stream"`
	DurationMs       int64  `json:"duration_ms"`
	PromptTokens     int64  `json:"prompt_tokens"`
	CompletionTokens int64  `json:"completion_tokens"`
	ReasoningTokens  int64  `json:"reasoning_tokens"`
	Error            string `json:"error"`
}

// LogBuffer 是保存最近 N 条请求日志的环形缓冲。
type LogBuffer struct {
	mu    sync.Mutex
	size  int
	items []RequestLog
	next  int
	count int
}

// NewLogBuffer 构造环形缓冲；size <= 0 时退化为不记录。
func NewLogBuffer(size int) *LogBuffer {
	if size < 0 {
		size = 0
	}
	return &LogBuffer{size: size}
}

// Add 追加一条日志。
func (b *LogBuffer) Add(entry RequestLog) {
	if b == nil || b.size == 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.items == nil {
		b.items = make([]RequestLog, b.size)
	}
	b.items[b.next] = entry
	b.next = (b.next + 1) % b.size
	if b.count < b.size {
		b.count++
	}
}

// Last 返回最近 n 条日志（最新在前）；n <= 0 或超过容量时返回全部。
func (b *LogBuffer) Last(n int) []RequestLog {
	if b == nil {
		return []RequestLog{}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if n <= 0 || n > b.count {
		n = b.count
	}
	result := make([]RequestLog, 0, n)
	for i := 0; i < n; i++ {
		index := (b.next - 1 - i + b.size*2) % b.size
		result = append(result, b.items[index])
	}
	return result
}

// nowRFC3339 返回当前时间的 RFC3339 字符串。
func nowRFC3339() string {
	return time.Now().Format(time.RFC3339)
}
