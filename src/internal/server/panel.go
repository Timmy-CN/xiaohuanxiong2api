package server

import (
	"net/http"

	"xhx2api/panel"
)

// handleAdminPage 处理 GET /admin 与 /admin/，返回内嵌的管理面板 HTML（SPEC §7.2）。
//
// 页面位于 src/panel/index.html，由 package panel 用 //go:embed 打进二进制；
// 这里逐字节写出，保证与仓库文件内容完全一致。
func (s *Server) handleAdminPage(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(panel.HTML())
}
