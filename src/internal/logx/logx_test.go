package logx

import (
	"bytes"
	"strings"
	"testing"
)

// TestParseLevel 覆盖合法/非法级别解析。
func TestParseLevel(t *testing.T) {
	cases := map[string]Level{
		"debug":  LevelDebug,
		"info":   LevelInfo,
		"warn":   LevelWarn,
		"error":  LevelError,
		"INFO":   LevelInfo,
		" Warn ": LevelWarn,
	}
	for input, want := range cases {
		got, err := ParseLevel(input)
		if err != nil {
			t.Fatalf("ParseLevel(%q) 返回错误: %v", input, err)
		}
		if got != want {
			t.Fatalf("ParseLevel(%q) = %v, 期望 %v", input, got, want)
		}
	}
	if _, err := ParseLevel("verbose"); err == nil {
		t.Fatal("非法级别应返回错误")
	} else if !strings.Contains(err.Error(), "debug/info/warn/error") {
		t.Fatalf("非法级别错误应提示可选值，实际: %v", err)
	}
}

// TestThresholdWrites 覆盖「阈值之上的写、阈值之下的不写」。
func TestThresholdWrites(t *testing.T) {
	var buf bytes.Buffer
	logger := New(&buf, "", LevelWarn)

	logger.Debugf("debug-%d", 1)
	logger.Infof("info-%d", 2)
	logger.Warnf("warn-%d", 3)
	logger.Errorf("error-%d", 4)
	logger.Printf("printf-%d", 5)

	out := buf.String()
	if strings.Contains(out, "debug-1") {
		t.Fatalf("warn 阈值下不应写 debug，实际: %s", out)
	}
	if strings.Contains(out, "info-2") {
		t.Fatalf("warn 阈值下不应写 info，实际: %s", out)
	}
	if strings.Contains(out, "printf-5") {
		t.Fatalf("warn 阈值下不应写 Printf(Info)，实际: %s", out)
	}
	if !strings.Contains(out, "warn-3") {
		t.Fatalf("warn 阈值下应写 warn，实际: %s", out)
	}
	if !strings.Contains(out, "error-4") {
		t.Fatalf("warn 阈值下应写 error，实际: %s", out)
	}
}

// TestDebugLevelWritesAll 覆盖 debug 阈值下全部输出。
func TestDebugLevelWritesAll(t *testing.T) {
	var buf bytes.Buffer
	logger := New(&buf, "", LevelDebug)
	logger.Debugf("d")
	logger.Infof("i")
	logger.Warnf("w")
	logger.Errorf("e")
	out := buf.String()
	for _, want := range []string{"d", "i", "w", "e"} {
		if !strings.Contains(out, want) {
			t.Fatalf("debug 阈值下应包含 %q，实际: %s", want, out)
		}
	}
}

// TestAlwaysfIgnoresThreshold 覆盖 SPEC3 §E1：Alwaysf 不受阈值影响。
func TestAlwaysfIgnoresThreshold(t *testing.T) {
	var buf bytes.Buffer
	logger := New(&buf, "", LevelError)

	logger.Infof("info-should-drop")
	logger.Alwaysf("always-%d", 1)

	out := buf.String()
	if strings.Contains(out, "info-should-drop") {
		t.Fatalf("error 阈值下 info 不应输出，实际: %s", out)
	}
	if !strings.Contains(out, "always-1") {
		t.Fatalf("Alwaysf 在 error 阈值下也必须输出，实际: %s", out)
	}
}

// TestBelowThresholdDoesNotFormat 覆盖「低于阈值不格式化」。
//
// 用一个实现 fmt.Stringer 的探针：只有真正走到格式化时 String() 才会被调用。
func TestBelowThresholdDoesNotFormat(t *testing.T) {
	var buf bytes.Buffer
	logger := New(&buf, "", LevelError)

	probe := &formatProbe{}
	logger.Debugf("probe=%s", probe)
	if probe.called {
		t.Fatal("低于阈值的调用不应格式化参数")
	}
	if buf.Len() != 0 {
		t.Fatalf("error 阈值下 debug 不应产生输出，实际: %s", buf.String())
	}

	// 阈值之上应真正格式化。
	above := New(&buf, "", LevelDebug)
	above.Debugf("probe=%s", probe)
	if !probe.called {
		t.Fatal("阈值之上的调用应格式化参数")
	}
}

// formatProbe 记录 String() 是否被调用。
type formatProbe struct{ called bool }

func (p *formatProbe) String() string {
	p.called = true
	return "probe"
}
