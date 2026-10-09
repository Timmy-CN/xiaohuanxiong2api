// Package version 保存构建版本号。
//
// 默认值是开发期版本；发布构建由 Dockerfile 通过
// -ldflags "-X xhx2api/internal/version.Version=<v>" 覆盖。
package version

// Version 是二进制版本号（可被 ldflags 覆盖）。
var Version = "0.2.0"
