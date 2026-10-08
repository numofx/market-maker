// Package logging keeps secrets out of the log stream.
//
// The RPC URL carries the provider key in its path (https://host/v2/<key>), the database URL a
// password, and Go's net/http embeds the full request URL in every transport error ("Post
// \"https://host/v2/<key>\": dial tcp ..."), so a key reaches the logs through any "error", err
// attribute on any failed call, not only where the URL is logged on purpose. The handler below
// redacts configured URLs wherever they appear in a record, and HostOnly is the label to log when
// a URL is meant to be shown.
package logging

import (
	"context"
	"log/slog"
	"net/url"
	"strings"
)

// HostOnly is the loggable form of a URL: scheme and host, never the path, query, fragment or
// userinfo. An empty input stays empty; an unparsable one is replaced wholesale rather than
// passed through.
func HostOnly(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "<redacted-url>"
	}
	return u.Scheme + "://" + u.Host
}

// NewRedactingHandler wraps inner so that every occurrence of any of the given URLs, in any
// string or error attribute at any group depth, is replaced by its HostOnly form before the
// record is written. A URL that is empty or already host-only is ignored.
func NewRedactingHandler(inner slog.Handler, urls ...string) slog.Handler {
	h := &redactingHandler{inner: inner}
	for _, raw := range urls {
		raw = strings.TrimSpace(raw)
		short := HostOnly(raw)
		if raw == "" || raw == short {
			continue
		}
		// A client may add or drop a trailing slash; the longer form is replaced first so the
		// shorter one does not leave a stray slash behind.
		if trimmed := strings.TrimSuffix(raw, "/"); trimmed != raw {
			h.rules = append(h.rules, rule{secret: raw, replacement: short}, rule{secret: trimmed, replacement: short})
		} else {
			h.rules = append(h.rules, rule{secret: raw + "/", replacement: short}, rule{secret: raw, replacement: short})
		}
	}
	return h
}

type rule struct{ secret, replacement string }

type redactingHandler struct {
	inner slog.Handler
	rules []rule
}

func (h *redactingHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

func (h *redactingHandler) Handle(ctx context.Context, record slog.Record) error {
	clean := slog.NewRecord(record.Time, record.Level, h.redactString(record.Message), record.PC)
	record.Attrs(func(a slog.Attr) bool {
		clean.AddAttrs(h.redactAttr(a))
		return true
	})
	return h.inner.Handle(ctx, clean)
}

func (h *redactingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	out := make([]slog.Attr, 0, len(attrs))
	for _, a := range attrs {
		out = append(out, h.redactAttr(a))
	}
	return &redactingHandler{inner: h.inner.WithAttrs(out), rules: h.rules}
}

func (h *redactingHandler) WithGroup(name string) slog.Handler {
	return &redactingHandler{inner: h.inner.WithGroup(name), rules: h.rules}
}

func (h *redactingHandler) redactString(s string) string {
	for _, r := range h.rules {
		s = strings.ReplaceAll(s, r.secret, r.replacement)
	}
	return s
}

// redactAttr rewrites the values a URL can hide in: strings, errors (and anything else whose
// text is what gets printed), and the members of a group. Numbers, times and booleans pass.
func (h *redactingHandler) redactAttr(a slog.Attr) slog.Attr {
	v := a.Value.Resolve()
	switch v.Kind() {
	case slog.KindString:
		return slog.String(a.Key, h.redactString(v.String()))
	case slog.KindGroup:
		members := v.Group()
		out := make([]any, 0, len(members))
		for _, m := range members {
			out = append(out, h.redactAttr(m))
		}
		return slog.Group(a.Key, out...)
	case slog.KindAny:
		switch x := v.Any().(type) {
		case error:
			return slog.String(a.Key, h.redactString(x.Error()))
		case string:
			return slog.String(a.Key, h.redactString(x))
		case []string:
			out := make([]string, len(x))
			for i, s := range x {
				out[i] = h.redactString(s)
			}
			return slog.Any(a.Key, out)
		case []any:
			// A slice of mixed values (the attrs... style used by some call sites): redact the
			// strings and errors inside it and leave the rest.
			out := make([]any, len(x))
			for i, item := range x {
				switch y := item.(type) {
				case string:
					out[i] = h.redactString(y)
				case error:
					out[i] = h.redactString(y.Error())
				default:
					out[i] = item
				}
			}
			return slog.Any(a.Key, out)
		default:
			// Whatever it is, it is printed with %v; redact that text rather than trust the type.
			if text := v.String(); text != h.redactString(text) {
				return slog.String(a.Key, h.redactString(text))
			}
			return a
		}
	default:
		return a
	}
}
