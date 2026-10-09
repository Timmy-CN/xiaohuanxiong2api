// Package panel 内嵌 §7.2 的管理面板页面。
//
// 页面是单文件、零外部依赖的暗色只读页，通过 //go:embed 打进二进制；
// 修改 index.html 后必须重新构建二进制才会生效（运行时不会读磁盘）。
package panel

import _ "embed"

//go:embed index.html
var indexHTML []byte

// HTML 返回内嵌面板页面的原始字节内容。
func HTML() []byte { return indexHTML }
