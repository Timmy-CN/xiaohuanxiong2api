package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"xhx2api/internal/upstream"
)

// findLog 在最近 n 条日志里查找第一条满足 ok 的记录（按最新在前顺序）。
func findLog(logs []RequestLog, ok func(RequestLog) bool) (RequestLog, bool) {
	for _, lg := range logs {
		if ok(lg) {
			return lg, true
		}
	}
	return RequestLog{}, false
}

// ── SPEC-A §6.1：未知模型也要落一条 400 日志 ──

func TestChatLogUnknownModel(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{
			"id": "c", "object": "chat.completion", "model": "internal",
			"choices": []map[string]any{{"index": 0, "message": map[string]any{"role": "assistant", "content": "ok"}}},
		})
	}
	env := newTestEnv(t, handler, []map[string]any{{
		"id": "a1", "access_token": "tok", "enabled": true, "priority": 1, "expires_at": 0,
	}}, nil)

	env.s.catalog.mu.Lock()
	env.s.catalog.models = []upstream.Model{{ID: "known-model", DisplayName: "已知", Visible: true}}
	env.s.catalog.source = "remote"
	env.s.catalog.mu.Unlock()

	resp, data := env.postChat(t, `{"model":"no-such-model","messages":[]}`)
	if resp.StatusCode != 400 {
		t.Fatalf("状态码 = %d, body=%s", resp.StatusCode, data)
	}
	lg, ok := findLog(env.s.logs.Last(10), func(lg RequestLog) bool {
		return lg.Status == 400 && strings.Contains(lg.Error, "未知模型") && lg.ClientModel == "no-such-model"
	})
	if !ok {
		t.Fatalf("日志里应有 400 未知模型记录（client_model=no-such-model），实际：%+v", env.s.logs.Last(10))
	}
	if lg.Model != "no-such-model" {
		t.Fatalf("日志 model = %q, 期望 no-such-model", lg.Model)
	}
}

// ── SPEC-A §6.2：非法 JSON 也要落一条 400 日志 ──

func TestChatLogInvalidJSON(t *testing.T) {
	env := newTestEnv(t, func(w http.ResponseWriter, r *http.Request) {}, nil, nil)

	resp, data := env.postChat(t, `{not-json`)
	if resp.StatusCode != 400 {
		t.Fatalf("状态码 = %d, body=%s", resp.StatusCode, data)
	}
	if _, ok := findLog(env.s.logs.Last(10), func(lg RequestLog) bool {
		return lg.Status == 400 && strings.Contains(lg.Error, "不是合法 JSON")
	}); !ok {
		t.Fatalf("日志里应有 400 非法 JSON 记录，实际：%+v", env.s.logs.Last(10))
	}
}

// ── SPEC-A §6.3：body 超限（413）也要落日志 ──

func TestChatLogBodyTooLarge(t *testing.T) {
	old := maxChatBodyBytes
	maxChatBodyBytes = 1024
	defer func() { maxChatBodyBytes = old }()

	env := newTestEnv(t, func(w http.ResponseWriter, r *http.Request) {}, nil, nil)

	resp, data := env.postChat(t, strings.Repeat("a", 4096))
	if resp.StatusCode != 413 {
		t.Fatalf("状态码 = %d, body=%s", resp.StatusCode, data)
	}
	lg, ok := findLog(env.s.logs.Last(10), func(lg RequestLog) bool {
		return lg.Status == 413
	})
	if !ok {
		t.Fatalf("日志里应有 413 记录，实际：%+v", env.s.logs.Last(10))
	}
	if lg.Error != "请求体超过 64MB 上限" {
		t.Fatalf("413 日志 error = %q, 期望「请求体超过 64MB 上限」", lg.Error)
	}
}

// ── SPEC-A §6.4：上游 5xx → 502，日志带 upstream_status ──

func TestChatLogUpstream500(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(500)
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 123, "message": "boom"})
	}
	env := newTestEnv(t, handler, []map[string]any{{
		"id": "a1", "access_token": "tok", "enabled": true, "priority": 1, "expires_at": 0,
	}}, nil)

	resp, data := env.postChat(t, `{"model":"sn-glm-5-3-flash","messages":[]}`)
	if resp.StatusCode != 502 {
		t.Fatalf("状态码 = %d, body=%s", resp.StatusCode, data)
	}
	lg, ok := findLog(env.s.logs.Last(10), func(lg RequestLog) bool {
		return lg.Status == 502
	})
	if !ok {
		t.Fatalf("日志里应有 502 记录，实际：%+v", env.s.logs.Last(10))
	}
	if lg.UpstreamStatus == nil || *lg.UpstreamStatus != 500 {
		t.Fatalf("upstream_status = %v, 期望 500", lg.UpstreamStatus)
	}
	if lg.UpstreamCode == nil || *lg.UpstreamCode != 123 {
		t.Fatalf("upstream_code = %v, 期望 123", lg.UpstreamCode)
	}
	if lg.Error == "" {
		t.Fatal("502 日志 error 不应为空")
	}
}

// ── SPEC-A §6.5：上游流中断（非干净 EOF）→ 日志 status=200 且 error 含「上游流中断」 ──

func TestChatLogStreamInterrupted(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			return
		}
		conn, buf, err := hj.Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		// 声明一个远大于实际写入的 Content-Length，然后直接断开连接，
		// 让客户端读到非干净 EOF（unexpected EOF），触发「上游流中断」分支。
		_, _ = buf.WriteString("HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nContent-Length: 100000\r\n\r\n")
		_, _ = buf.WriteString("data: {\"id\":\"c\",\"model\":\"raccoon-internal\",\"choices\":[]}\n\n")
		_ = buf.Flush()
		// 不写 [DONE]，直接关闭底层连接。
	}
	env := newTestEnv(t, handler, []map[string]any{{
		"id": "a1", "access_token": "tok", "enabled": true, "priority": 1, "expires_at": 0,
	}}, nil)

	resp, data := env.postChat(t, `{"model":"glm-5.3-flash","messages":[],"stream":true}`)
	if resp.StatusCode != 200 {
		t.Fatalf("状态码 = %d, body=%s", resp.StatusCode, data)
	}
	logs := env.s.logs.Last(1)
	if len(logs) != 1 {
		t.Fatalf("请求日志条数 = %d", len(logs))
	}
	if logs[0].Status != 200 {
		t.Fatalf("流中断日志 status = %d, 期望 200", logs[0].Status)
	}
	if !strings.Contains(logs[0].Error, "上游流中断") {
		t.Fatalf("流中断日志 error = %q, 期望含「上游流中断」", logs[0].Error)
	}
}
