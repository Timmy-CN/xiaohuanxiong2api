// Package upstream 封装对小浣熊（商汤 Raccoon）上游的 HTTP 调用。
//
// 包含：共享 http.Client 构造、聊天转发、模型目录、token 刷新、积分/订阅查询。
// 所有响应体读取都走 io.LimitReader（目录/积分 4MB，错误体 64KB）；
// SSE 流不设上限，按行读取（见 server/chat.go）。
package upstream

import (
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// 响应体读取上限（SPEC §4.6）。
const (
	maxCatalogBytes = 4 << 20
	maxErrorBytes   = 64 << 10
)

// Options 是构造客户端所需的参数（由 §3 配置换算而来）。
type Options struct {
	LLMBase               string
	AuthBase              string
	PointsBase            string
	Origin                string
	ConnectTimeout        time.Duration
	ResponseHeaderTimeout time.Duration
	RequestTimeout        time.Duration
	RefreshTimeout        time.Duration
	CatalogTimeout        time.Duration
	BalanceTimeout        time.Duration
	BillsTimeout          time.Duration
	InsecureSkipVerify    bool
	UserAgent             string
	ExtraHeaders          map[string]string
}

// Client 是共享的上游 HTTP 客户端。
type Client struct {
	http *http.Client
	opts Options
}

// NewClient 构造共享客户端。
//
// 注意：**不**设置 http.Client.Timeout（会截断长流式回答）；总超时按请求类型用
// context.WithTimeout 套（见各方法）。DialContext 用 connect_timeout；
// ResponseHeaderTimeout 用 response_header_timeout；TLS 校验开关来自配置。
func NewClient(opts Options) *Client {
	dialer := &net.Dialer{Timeout: opts.ConnectTimeout}
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   10,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
		ResponseHeaderTimeout: opts.ResponseHeaderTimeout,
		TLSClientConfig:       &tls.Config{InsecureSkipVerify: opts.InsecureSkipVerify}, //nolint:gosec // 由配置显式控制
	}
	return &Client{
		http: &http.Client{Transport: transport},
		opts: opts,
	}
}

// LLMBase 返回 LLM 网关基址。
func (c *Client) LLMBase() string { return c.opts.LLMBase }

// AuthBase 返回鉴权 API 基址。
func (c *Client) AuthBase() string { return c.opts.AuthBase }

// PointsBase 返回积分 API 基址。
func (c *Client) PointsBase() string { return c.opts.PointsBase }

// applyCommonHeaders 设置所有出网请求共用的头：User-Agent + extra_headers。
func (c *Client) applyCommonHeaders(req *http.Request) {
	if c.opts.UserAgent != "" {
		req.Header.Set("User-Agent", c.opts.UserAgent)
	}
	for key, value := range c.opts.ExtraHeaders {
		req.Header.Set(key, value)
	}
}

// doChat 发送聊天请求，返回原始 *http.Response（不读取 body，交给调用方流式处理）。
//
// 请求头 = Authorization + Content-Type + Accept + user_agent + extra_headers。
// **不**透传客户端其它头（SPEC §4.2）。
func (c *Client) doChat(ctx context.Context, token string, body []byte) (*http.Response, error) {
	url := c.opts.LLMBase + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "*/*")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	c.applyCommonHeaders(req)
	return c.http.Do(req)
}

// ChatStream 转发流式聊天请求（返回原始响应，body 由调用方按行读取）。
func (c *Client) ChatStream(ctx context.Context, token string, body []byte) (*http.Response, error) {
	return c.doChat(ctx, token, body)
}

// ChatJSON 转发非流式聊天请求（返回原始响应）。
func (c *Client) ChatJSON(ctx context.Context, token string, body []byte) (*http.Response, error) {
	return c.doChat(ctx, token, body)
}

// doJSON 发一次请求并读取响应体（受 limit 限制）。timeout > 0 时套请求级总超时。
func (c *Client) doJSON(ctx context.Context, method, url string, body []byte, token string, headers map[string]string, timeout time.Duration, limit int64) (int, []byte, error) {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return 0, nil, err
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	c.applyCommonHeaders(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if err != nil {
		return resp.StatusCode, data, err
	}
	return resp.StatusCode, data, nil
}

// jsonMessage 从上游错误体里尽量取出人类可读文案。
func jsonMessage(data []byte) string {
	msg := extractMessage(data)
	if msg == "" {
		text := strings.TrimSpace(string(data))
		if len(text) > 200 {
			text = text[:200]
		}
		return text
	}
	return msg
}
