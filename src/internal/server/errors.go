package server

import (
	"encoding/json"
	"net/http"
)

// 错误 type 取值（SPEC §6.5）。
const (
	typeInvalidRequest = "invalid_request_error"
	typeAuthentication = "authentication_error"
	typeRateLimit      = "rate_limit_error"
	typeUpstream       = "upstream_error"
	typeNoAccount      = "no_account"
)

// apiError 是错误信封的 error 字段。
type apiError struct {
	Message        string `json:"message"`
	Type           string `json:"type"`
	Code           string `json:"code,omitempty"`
	UpstreamStatus *int   `json:"upstream_status,omitempty"`
	UpstreamCode   *int   `json:"upstream_code,omitempty"`
}

type errorEnvelope struct {
	Error apiError `json:"error"`
}

// writeError 写出统一错误信封（SPEC §6.5）。
func writeError(w http.ResponseWriter, status int, err apiError) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorEnvelope{Error: err})
}

// writeJSON 写出 2xx JSON。
func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
