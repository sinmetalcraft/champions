// Package httpx は JSON API を書くための小さなヘルパを提供する。
package httpx

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
)

// Error は HTTP のステータスコードを持つエラー。ハンドラから返すとそのステータスで応答する。
type Error struct {
	Code    int
	Message string
	Err     error
}

func (e *Error) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s: %v", e.Message, e.Err)
	}
	return e.Message
}

func (e *Error) Unwrap() error { return e.Err }

// Errorf は指定したステータスコードの *Error を作る。
func Errorf(code int, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// WrapError は err を指定したステータスコードの *Error で包む。
func WrapError(code int, err error, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...), Err: err}
}

// Handler はエラーを返せる http.Handler。
type Handler func(w http.ResponseWriter, r *http.Request) error

// ServeHTTP は Handler が返したエラーを JSON のエラーレスポンスに変換する。
func (h Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if err := h(w, r); err != nil {
		var he *Error
		if !errors.As(err, &he) {
			he = &Error{Code: http.StatusInternalServerError, Message: "internal server error", Err: err}
		}
		if he.Code >= http.StatusInternalServerError {
			slog.Error("request failed", "method", r.Method, "path", r.URL.Path, "status", he.Code, "error", err.Error())
		} else {
			slog.Info("request rejected", "method", r.Method, "path", r.URL.Path, "status", he.Code, "error", err.Error())
		}
		WriteJSON(w, he.Code, map[string]string{"error": he.Message})
	}
}

// WriteJSON は v を JSON にして書き出す。
func WriteJSON(w http.ResponseWriter, code int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		slog.Error("failed to marshal response", "error", err)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"failed to marshal response"}`))
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_, _ = w.Write(body)
}

// DecodeJSON はリクエストボディを v にデコードする。未知のフィールドはエラーにする。
func DecodeJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return WrapError(http.StatusBadRequest, err, "invalid request body")
	}
	return nil
}
