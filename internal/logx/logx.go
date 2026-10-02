// Package logx は Google Cloud Logging 向けの構造化ログとトレース相関のヘルパを提供する。
package logx

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
)

type traceKey struct{}

// TraceInfo は Cloud Trace / W3C Trace Context から抽出したトレース情報。
type TraceInfo struct {
	TraceID string
	SpanID  string
	Sampled bool
}

// FromContext は context から TraceInfo を取り出す。
func FromContext(ctx context.Context) *TraceInfo {
	if t, ok := ctx.Value(traceKey{}).(*TraceInfo); ok {
		return t
	}
	return nil
}

// WithTrace は context に TraceInfo を格納する。
func WithTrace(ctx context.Context, t *TraceInfo) context.Context {
	return context.WithValue(ctx, traceKey{}, t)
}

// ParseTrace は HTTP リクエストヘッダーからトレース情報を抽出する。
// X-Cloud-Trace-Context と traceparent (W3C) の両方に対応する。
func ParseTrace(r *http.Request) *TraceInfo {
	if header := r.Header.Get("X-Cloud-Trace-Context"); header != "" {
		return parseCloudTraceContext(header)
	}
	if header := r.Header.Get("traceparent"); header != "" {
		return parseTraceparent(header)
	}
	return nil
}

func parseCloudTraceContext(header string) *TraceInfo {
	// X-Cloud-Trace-Context: TRACE_ID[/SPAN_ID][;o=TRACE_TRUE]
	// 例: 105445aa7843bc8bf206b120001000/1;o=1
	parts := strings.SplitN(header, ";", 2)
	sampled := false
	if len(parts) > 1 {
		sampled = strings.Contains(parts[1], "o=1")
	}

	traceAndSpan := parts[0]
	slash := strings.IndexByte(traceAndSpan, '/')
	if slash == -1 {
		return &TraceInfo{TraceID: traceAndSpan, Sampled: sampled}
	}

	traceID := traceAndSpan[:slash]
	rawSpan := traceAndSpan[slash+1:]
	spanID := rawSpan
	// Cloud Logging の spanId は 16 桁の 16 進数文字列を期待するため変換する。
	if spanNum, err := strconv.ParseUint(rawSpan, 10, 64); err == nil {
		spanID = fmt.Sprintf("%016x", spanNum)
	}

	return &TraceInfo{
		TraceID: traceID,
		SpanID:  spanID,
		Sampled: sampled,
	}
}

func parseTraceparent(header string) *TraceInfo {
	// traceparent: 00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01
	parts := strings.Split(header, "-")
	if len(parts) < 4 {
		return nil
	}
	return &TraceInfo{
		TraceID: parts[1],
		SpanID:  parts[2],
		Sampled: parts[3] == "01",
	}
}

// TraceMiddleware は HTTP リクエストのヘッダーからトレース情報を取得し、Context に乗せる。
func TraceMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t := ParseTrace(r)
		if t != nil {
			r = r.WithContext(WithTrace(r.Context(), t))
		}
		next.ServeHTTP(w, r)
	})
}

// Handler は Google Cloud Logging 向けの slog.Handler ラッパー。
// context に含まれる TraceInfo から logging.googleapis.com/trace などの特殊属性を付与する。
type Handler struct {
	parent    slog.Handler
	projectID string
}

// NewHandler は slog.Handler を Google Cloud Logging 用にラップする。
func NewHandler(parent slog.Handler, projectID string) *Handler {
	return &Handler{
		parent:    parent,
		projectID: projectID,
	}
}

func (h *Handler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.parent.Enabled(ctx, level)
}

func (h *Handler) Handle(ctx context.Context, r slog.Record) error {
	if t := FromContext(ctx); t != nil && t.TraceID != "" {
		if h.projectID != "" {
			r.AddAttrs(slog.String("logging.googleapis.com/trace", fmt.Sprintf("projects/%s/traces/%s", h.projectID, t.TraceID)))
		}
		if t.SpanID != "" {
			r.AddAttrs(slog.String("logging.googleapis.com/spanId", t.SpanID))
		}
		if t.Sampled {
			r.AddAttrs(slog.Bool("logging.googleapis.com/trace_sampled", true))
		}
	}
	return h.parent.Handle(ctx, r)
}

func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &Handler{
		parent:    h.parent.WithAttrs(attrs),
		projectID: h.projectID,
	}
}

func (h *Handler) WithGroup(name string) slog.Handler {
	return &Handler{
		parent:    h.parent.WithGroup(name),
		projectID: h.projectID,
	}
}
