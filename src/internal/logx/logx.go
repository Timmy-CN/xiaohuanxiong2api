// Package logx 提供带阈值过滤的分级日志器（FIX F1）。
//
// 低于阈值的调用会被直接丢弃（不会先格式化），从而让 log.level 真正生效。
package logx

import (
	"fmt"
	"io"
	"log"
	"strings"
)

// Level 是日志级别，数值越大越严重。
type Level int

const (
	LevelDebug Level = iota
	LevelInfo
	LevelWarn
	LevelError
)

// String 返回级别的规范小写名。
func (l Level) String() string {
	switch l {
	case LevelDebug:
		return "debug"
	case LevelInfo:
		return "info"
	case LevelWarn:
		return "warn"
	case LevelError:
		return "error"
	default:
		return "info"
	}
}

// ParseLevel 解析级别字符串；非法值返回中文 error。
//
// 注意：配置校验仍用 config 里的枚举（FIX 明确不要改），这里的解析只服务于日志器构造。
func ParseLevel(s string) (Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return LevelDebug, nil
	case "info":
		return LevelInfo, nil
	case "warn":
		return LevelWarn, nil
	case "error":
		return LevelError, nil
	default:
		return LevelInfo, fmt.Errorf("非法日志级别 %q：只能是 debug/info/warn/error", s)
	}
}

// Logger 是带级别的日志器，内部委托给标准库 *log.Logger。
type Logger struct {
	inner *log.Logger
	level Level
}

// New 构造日志器：prefix 传给标准库 log.New，时间戳沿用 log.LstdFlags。
func New(w io.Writer, prefix string, level Level) *Logger {
	return &Logger{inner: log.New(w, prefix, log.LstdFlags), level: level}
}

// logf 是内部统一入口：先比阈值，低于阈值直接返回（不格式化）。
func (l *Logger) logf(level Level, format string, args ...any) {
	if l == nil || l.inner == nil || level < l.level {
		return
	}
	l.inner.Printf(format, args...)
}

// Debugf 打印 debug 级日志。
func (l *Logger) Debugf(format string, args ...any) { l.logf(LevelDebug, format, args...) }

// Infof 打印 info 级日志。
func (l *Logger) Infof(format string, args ...any) { l.logf(LevelInfo, format, args...) }

// Warnf 打印 warn 级日志。
func (l *Logger) Warnf(format string, args ...any) { l.logf(LevelWarn, format, args...) }

// Errorf 打印 error 级日志。
func (l *Logger) Errorf(format string, args ...any) { l.logf(LevelError, format, args...) }

// Printf 等价于 Infof，保留给既有调用点以便平滑迁移。
func (l *Logger) Printf(format string, args ...any) { l.logf(LevelInfo, format, args...) }

// Alwaysf 无条件打印一行，不受日志阈值影响（SPEC3 §E1）。
//
// 用于「生效配置」「版本/监听/账号/目录摘要」这类运维必需的启动证据：
// 即便 log.level=error，这些行也必须出现在 stdout。其余日志照旧受阈值控制。
func (l *Logger) Alwaysf(format string, args ...any) {
	if l == nil || l.inner == nil {
		return
	}
	l.inner.Printf(format, args...)
}
