package main

import (
	"io"
	"log/slog"
)

// newLogger builds a slog logger that renders every time value in cfg.Location.
func newLogger(cfg Config, w io.Writer) *slog.Logger {
	opts := &slog.HandlerOptions{
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			if a.Value.Kind() == slog.KindTime && cfg.Location != nil {
				a.Value = slog.TimeValue(a.Value.Time().In(cfg.Location))
			}
			return a
		},
	}
	if cfg.LogFormat == "json" {
		return slog.New(slog.NewJSONHandler(w, opts))
	}
	return slog.New(slog.NewTextHandler(w, opts))
}
