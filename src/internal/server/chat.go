package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const maxChatBodyBytes = 64 << 20

// handleChat 实现 POST /v1/chat/completions（SPEC §6.3）。
func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxChatBodyBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, apiError{
				Message: "请求体超过 64MB 上限",
				Type:    typeInvalidRequest,
				Code:    "request_too_large",
			})
			return
		}
		writeError(w, http.StatusBadRequest, apiError{
			Message: "读取请求体失败：" + err.Error(),
			Type:    typeInvalidRequest,
		})
		return
	}

	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal(body, &fields); err != nil {
		writeError(w, http.StatusBadRequest, apiError{
			Message: "请求体不是合法 JSON：" + err.Error(),
			Type:    typeInvalidRequest,
			Code:    "invalid_json",
		})
		return
	}

	// 模型解析：缺省回填默认模型，其余原样透传给上游（SPEC4 §F.2.3）。
	clientModel := ""
	if raw, ok := fields["model"]; ok {
		_ = json.Unmarshal(raw, &clientModel)
	}
	resolvedClient := clientModel
	if resolvedClient == "" {
		resolvedClient = s.cfg.Models.DefaultModelValue()
	}
	upstreamModel := resolvedClient

	// 模型校验（SPEC §6.3.3）。
	if s.cfg.Models.ValidateValue() == "catalog" {
		if s.catalog.Count() == 0 {
			s.logger.Warnf("[chat] 警告：模型目录为空，跳过模型校验（model=%s）", resolvedClient)
		} else if !s.catalog.Has(upstreamModel) {
			s.logger.Warnf("[chat] 未知模型 %s，拒绝请求（可用模型见 /v1/models）", resolvedClient)
			writeError(w, http.StatusBadRequest, apiError{
				Message: "未知模型 " + resolvedClient + "，可用模型见 /v1/models",
				Type:    typeInvalidRequest,
				Code:    "model_not_found",
			})
			return
		}
	}

	stream := false
	if raw, ok := fields["stream"]; ok {
		_ = json.Unmarshal(raw, &stream)
	}

	// 只替换 model 键，其余字段原样保留（SPEC §6.3.4）。
	modelJSON, err := json.Marshal(upstreamModel)
	if err != nil {
		writeError(w, http.StatusInternalServerError, apiError{Message: "序列化模型名失败", Type: typeInvalidRequest})
		return
	}
	fields["model"] = json.RawMessage(modelJSON)
	upstreamBody, err := json.Marshal(fields)
	if err != nil {
		writeError(w, http.StatusInternalServerError, apiError{Message: "序列化请求体失败", Type: typeInvalidRequest})
		return
	}

	entry := RequestLog{
		Time:        nowRFC3339(),
		Method:      http.MethodPost,
		Path:        "/v1/chat/completions",
		Model:       upstreamModel,
		ClientModel: resolvedClient,
		Stream:      stream,
	}
	started := time.Now()
	defer func() {
		entry.DurationMs = time.Since(started).Milliseconds()
		s.logs.Add(entry)
		if s.cfg.Log.AccessLogEnabled() {
			s.logger.Infof("[access] %s %s model=%s client_model=%s account=%s status=%d stream=%v duration_ms=%d prompt=%d completion=%d reasoning=%d error=%s",
				entry.Method, entry.Path, entry.Model, entry.ClientModel, entry.Account, entry.Status, entry.Stream,
				entry.DurationMs, entry.PromptTokens, entry.CompletionTokens, entry.ReasoningTokens, entry.Error)
		}
	}()

	// 请求级总超时（0 = 不设，长回答流式）。
	ctx := r.Context()
	if timeout := s.cfg.Upstream.RequestTimeout(); timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	maxAttempts := s.cfg.Accounts.MaxAttemptsValue()
	tried := map[string]bool{}
	lastStatus := 0
	var lastErr apiError
	attempted := false

	for attempt := 0; attempt < maxAttempts; attempt++ {
		account := s.pool.Pick(tried)
		if account == nil {
			break
		}
		attempted = true
		tried[account.ID] = true
		entry.Account = account.ID
		s.logger.Debugf("[chat] 选到账号 %s（第 %d 次尝试，model=%s）", account.ID, attempt+1, upstreamModel)

		token, err := s.pool.Token(ctx, account.ID, false)
		if err != nil {
			s.logger.Warnf("[chat] 账号 %s 刷新失败：%v", account.ID, err)
			lastErr = apiError{Message: "账号 " + account.ID + " 刷新失败：" + err.Error(), Type: typeUpstream, Code: "token_refresh_failed"}
			lastStatus = http.StatusBadGateway
			continue
		}
		resp, err := s.sendChat(ctx, token, upstreamBody, stream)
		if err != nil {
			lastErr = apiError{Message: "转发上游失败：" + err.Error(), Type: typeUpstream, Code: "upstream_network"}
			lastStatus = http.StatusBadGateway
			continue
		}

		// 401 → 强制刷新同账号并重试一次（不计入 max_attempts，SPEC §5.3）。
		if resp.StatusCode == http.StatusUnauthorized && s.cfg.Accounts.RefreshOn401Value() {
			_ = resp.Body.Close()
			token, err = s.pool.Token(ctx, account.ID, true)
			if err != nil {
				s.pool.MarkError(account.ID, err.Error())
				s.logger.Warnf("[chat] 账号 %s 强制刷新失败：%v", account.ID, err)
				lastErr = apiError{Message: "账号 " + account.ID + " 强制刷新失败：" + err.Error(), Type: typeUpstream, Code: "token_refresh_failed"}
				lastStatus = http.StatusBadGateway
				continue
			}
			resp, err = s.sendChat(ctx, token, upstreamBody, stream)
			if err != nil {
				lastErr = apiError{Message: "转发上游失败：" + err.Error(), Type: typeUpstream, Code: "upstream_network"}
				lastStatus = http.StatusBadGateway
				continue
			}
		}

		switch {
		case resp.StatusCode >= 200 && resp.StatusCode < 300:
			if stream {
				s.forwardStream(ctx, w, resp, &entry, resolvedClient)
			} else {
				s.forwardJSON(w, resp, &entry, resolvedClient)
			}
			return
		case resp.StatusCode == http.StatusUnauthorized:
			errBody := readErrorBody(resp)
			s.pool.MarkError(account.ID, "上游返回 401")
			lastErr = buildUpstreamError(http.StatusUnauthorized, errBody)
			lastStatus = http.StatusBadGateway
			continue
		case resp.StatusCode == http.StatusTooManyRequests:
			_ = resp.Body.Close()
			s.pool.Cooldown(account.ID)
			s.logger.Warnf("[chat] 账号 %s 收到 429，进入冷却 %s", account.ID, s.cfg.Accounts.Cooldown())
			lastErr = buildUpstreamError(http.StatusTooManyRequests, nil)
			lastStatus = http.StatusTooManyRequests
			continue
		case resp.StatusCode >= 500:
			errBody := readErrorBody(resp)
			lastErr = buildUpstreamError(resp.StatusCode, errBody)
			lastStatus = http.StatusBadGateway
			continue
		default:
			// 其它 4xx：不换号，原样透传状态码与上游 message（SPEC §5.4）。
			errBody := readErrorBody(resp)
			upErr := buildUpstreamError(resp.StatusCode, errBody)
			entry.Status = resp.StatusCode
			entry.Error = upErr.Message
			writeError(w, resp.StatusCode, upErr)
			return
		}
	}

	if !attempted {
		entry.Status = http.StatusServiceUnavailable
		entry.Error = "no_account"
		writeError(w, http.StatusServiceUnavailable, apiError{
			Message: "没有可用账号：请在 data/accounts.json 里配置小浣熊账号",
			Type:    typeNoAccount,
			Code:    "no_account",
		})
		return
	}

	if lastStatus == http.StatusTooManyRequests {
		w.Header().Set("Retry-After", strconv.Itoa(int(s.cfg.Accounts.Cooldown().Seconds())))
		entry.Status = http.StatusTooManyRequests
		entry.Error = lastErr.Message
		writeError(w, http.StatusTooManyRequests, lastErr)
		return
	}
	if lastErr.Message == "" {
		lastErr = apiError{Message: "上游请求失败", Type: typeUpstream, Code: "upstream_error"}
	}
	entry.Status = http.StatusBadGateway
	entry.Error = lastErr.Message
	writeError(w, http.StatusBadGateway, lastErr)
}

// sendChat 转发一次聊天请求。
func (s *Server) sendChat(ctx context.Context, token string, body []byte, stream bool) (*http.Response, error) {
	if stream {
		return s.client.ChatStream(ctx, token, body)
	}
	return s.client.ChatJSON(ctx, token, body)
}

// readErrorBody 读取上游错误体（限 64KB）。
func readErrorBody(resp *http.Response) []byte {
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	return data
}

// buildUpstreamError 把上游错误体转成错误信封（含状态码与上游 message/code）。
func buildUpstreamError(status int, body []byte) apiError {
	message := "上游返回 " + strconv.Itoa(status)
	var payload struct {
		Code    *int   `json:"code"`
		Message string `json:"message"`
		Details string `json:"details"`
	}
	if len(body) > 0 {
		_ = json.Unmarshal(body, &payload)
	}
	detail := payload.Message
	if detail == "" {
		detail = payload.Details
	}
	if detail != "" {
		message += "：" + detail
	}
	err := apiError{
		Message:        message,
		Type:           typeUpstream,
		Code:           "upstream_" + strconv.Itoa(status),
		UpstreamStatus: &status,
	}
	if status == http.StatusTooManyRequests {
		err.Type = typeRateLimit
	}
	if payload.Code != nil {
		err.UpstreamCode = payload.Code
	}
	return err
}

// usageInfo 对应上游的 usage 字段。
type usageInfo struct {
	PromptTokens            int64 `json:"prompt_tokens"`
	CompletionTokens        int64 `json:"completion_tokens"`
	CompletionTokensDetails struct {
		ReasoningTokens int64 `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

func applyUsage(entry *RequestLog, raw json.RawMessage) {
	if len(raw) == 0 {
		return
	}
	var usage usageInfo
	if err := json.Unmarshal(raw, &usage); err != nil {
		return
	}
	entry.PromptTokens = usage.PromptTokens
	entry.CompletionTokens = usage.CompletionTokens
	entry.ReasoningTokens = usage.CompletionTokensDetails.ReasoningTokens
}

// forwardStream 逐行透传上游 SSE，并把每帧的 model 改写为客户端请求名。
func (s *Server) forwardStream(ctx context.Context, w http.ResponseWriter, resp *http.Response, entry *RequestLog, clientModel string) {
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("X-Xhx-Model", entry.Model)
	w.Header().Set("X-Xhx-Account", entry.Account)
	entry.Status = resp.StatusCode
	w.WriteHeader(resp.StatusCode)

	flusher, _ := w.(http.Flusher)
	reader := bufio.NewReader(resp.Body)
	frames := 0
	for {
		line, err := reader.ReadString('\n')
		if len(line) > 0 {
			if strings.HasPrefix(strings.TrimSpace(line), "data:") {
				frames++
			}
			s.writeSSELine(w, flusher, line, clientModel, entry)
		}
		if err != nil {
			if err != io.EOF && ctx.Err() == nil {
				s.logger.Warnf("[chat] 上游流中断：%v", err)
			}
			break
		}
		if ctx.Err() != nil {
			break
		}
	}
	s.logger.Debugf("[chat] 流式结束：共 %d 个 data 帧（model=%s account=%s）", frames, clientModel, entry.Account)
}

// writeSSELine 处理并写出一行 SSE。
func (s *Server) writeSSELine(w io.Writer, flusher http.Flusher, line, clientModel string, entry *RequestLog) {
	trimmed := strings.TrimRight(line, "\r\n")
	if !strings.HasPrefix(trimmed, "data:") {
		_, _ = io.WriteString(w, line)
		if flusher != nil {
			flusher.Flush()
		}
		return
	}
	payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
	if payload == "" || payload == "[DONE]" {
		_, _ = io.WriteString(w, line)
		if flusher != nil {
			flusher.Flush()
		}
		return
	}
	frame := map[string]json.RawMessage{}
	if err := json.Unmarshal([]byte(payload), &frame); err != nil {
		// 无法解析的 data: 行原样透传。
		_, _ = io.WriteString(w, line)
		if flusher != nil {
			flusher.Flush()
		}
		return
	}
	if raw, ok := frame["usage"]; ok {
		applyUsage(entry, raw)
	}
	if modelJSON, err := json.Marshal(clientModel); err == nil {
		frame["model"] = json.RawMessage(modelJSON)
	}
	rewritten, err := json.Marshal(frame)
	if err != nil {
		_, _ = io.WriteString(w, line)
	} else {
		_, _ = io.WriteString(w, "data: "+string(rewritten)+"\n")
	}
	if flusher != nil {
		flusher.Flush()
	}
}

// forwardJSON 改写非流式响应体的 model 后原样下发。
func (s *Server) forwardJSON(w http.ResponseWriter, resp *http.Response, entry *RequestLog, clientModel string) {
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxChatBodyBytes))
	if err != nil {
		entry.Status = http.StatusBadGateway
		entry.Error = "读取上游响应失败：" + err.Error()
		writeError(w, http.StatusBadGateway, apiError{Message: entry.Error, Type: typeUpstream, Code: "upstream_read_error"})
		return
	}
	entry.Status = resp.StatusCode
	w.Header().Set("X-Xhx-Model", entry.Model)
	w.Header().Set("X-Xhx-Account", entry.Account)

	frame := map[string]json.RawMessage{}
	if err := json.Unmarshal(data, &frame); err != nil {
		w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(data)
		return
	}
	if raw, ok := frame["usage"]; ok {
		applyUsage(entry, raw)
	}
	if modelJSON, err := json.Marshal(clientModel); err == nil {
		frame["model"] = json.RawMessage(modelJSON)
	}
	rewritten, err := json.Marshal(frame)
	if err != nil {
		w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(data)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(rewritten)
}
