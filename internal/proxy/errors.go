package proxy

import (
	"encoding/json"
	"net/http"
)

type APIError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code"`
	Param   string `json:"param,omitempty"`
}

type errorEnvelope struct {
	Error APIError `json:"error"`
}

func WriteError(w http.ResponseWriter, status int, err APIError) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorEnvelope{Error: err})
}

func InvalidRequest(msg, code string) APIError {
	return APIError{Message: msg, Type: "invalid_request_error", Code: code}
}

func Unauthorized(msg, code string) APIError {
	return APIError{Message: msg, Type: "authentication_error", Code: code}
}

func PermissionError(msg, code string) APIError {
	return APIError{Message: msg, Type: "permission_error", Code: code}
}

func NotFound(msg, code string) APIError {
	return APIError{Message: msg, Type: "invalid_request_error", Code: code}
}

func RateLimit(msg, code string) APIError {
	return APIError{Message: msg, Type: "rate_limit_error", Code: code}
}

func BackendUnavailable(msg, code string) APIError {
	return APIError{Message: msg, Type: "backend_unavailable", Code: code}
}

func InternalError(msg, code string) APIError {
	return APIError{Message: msg, Type: "internal_error", Code: code}
}
