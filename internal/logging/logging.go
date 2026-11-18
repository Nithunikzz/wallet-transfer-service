// Package logging carries a request id through context.Context and attaches it
// to every slog record logged with that context.
package logging

import (
	"context"
	"log/slog"
)

type ctxKey struct{}

// WithRequestID returns a context carrying the request id.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxKey{}, id)
}

// RequestID returns the request id stored in ctx, or "".
func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(ctxKey{}).(string)
	return id
}

// ContextHandler adds request_id to records logged via the *Context methods
// (InfoContext, ErrorContext, ...), so layers below the handler never have to
// pass the id around explicitly.
type ContextHandler struct {
	slog.Handler
}

// NewLogger wraps h so request ids from context are attached automatically.
func NewLogger(h slog.Handler) *slog.Logger {
	return slog.New(ContextHandler{Handler: h})
}

func (h ContextHandler) Handle(ctx context.Context, r slog.Record) error {
	if id := RequestID(ctx); id != "" {
		r.AddAttrs(slog.String("request_id", id))
	}
	return h.Handler.Handle(ctx, r)
}

func (h ContextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return ContextHandler{Handler: h.Handler.WithAttrs(attrs)}
}

func (h ContextHandler) WithGroup(name string) slog.Handler {
	return ContextHandler{Handler: h.Handler.WithGroup(name)}
}
