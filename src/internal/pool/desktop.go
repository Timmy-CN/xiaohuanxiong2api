package pool

// 只读复用官方桌面客户端（~/.box-agent/config/auth.json）的登录态。
//
// 「活席」语义照 magpie raccoon 插件（#166 实测）：桌面源账号在每次取 token
// 前先同步本机登录态，只接管同一账号的更新 token；绝不读取、更不动用桌面端
// 自己的 refresh_token（那会轮换掉客户端会话）。

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"xhx2api/internal/upstream"
)

// desktopAuthMaxBytes 是 auth.json 的大小上限（正常只有几百字节，超限视为非法）。
const desktopAuthMaxBytes = 256 * 1024

// DesktopAuthPath 返回桌面端登录态文件路径；环境变量
// RACCOON_BOX_AGENT_CONFIG_DIR / BOX_AGENT_CONFIG_DIR 可覆盖所在目录。
func DesktopAuthPath() string {
	for _, key := range []string{"RACCOON_BOX_AGENT_CONFIG_DIR", "BOX_AGENT_CONFIG_DIR"} {
		if dir := strings.TrimSpace(os.Getenv(key)); dir != "" {
			return filepath.Join(dir, "auth.json")
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".box-agent", "config", "auth.json")
	}
	return filepath.Join(home, ".box-agent", "config", "auth.json")
}

// DesktopAuth 是桌面端登录态文件里可用的部分。
type DesktopAuth struct {
	AccessToken string
	ExpiresAt   int64 // 毫秒；解不出为 0
	UserID      string
}

// ReadDesktopAuth 只读解析桌面端登录态。
//
// 文件缺失返回包着 os.ErrNotExist 的错误；超限/非法/无 access_token 返回普通
// 错误（文案含路径）。只取 access_token 及其 JWT 里的 uid/expiry。
func ReadDesktopAuth() (*DesktopAuth, error) {
	path := DesktopAuthPath()
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("读取桌面端登录态失败: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > desktopAuthMaxBytes {
		return nil, fmt.Errorf("桌面端登录态文件异常（%s，%d 字节）", path, info.Size())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取桌面端登录态失败（%s）：%w", path, err)
	}
	var parsed struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return nil, fmt.Errorf("解析桌面端登录态失败（%s）：%w", path, err)
	}
	access := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(parsed.AccessToken), "Bearer "))
	if access == "" {
		return nil, fmt.Errorf("桌面端登录态里没有 access_token（%s，客户端可能未登录）", path)
	}
	return &DesktopAuth{
		AccessToken: access,
		ExpiresAt:   upstream.JWTExpiry(access),
		UserID:      upstream.JWTUserID(access),
	}, nil
}
