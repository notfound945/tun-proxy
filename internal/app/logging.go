package app

import (
	"log/slog"
	"os"

	"github.com/hailinpan/tun-proxy/internal/config"
)

func configureLogging(configuration config.Log) {
	level := slog.LevelInfo
	switch configuration.Level {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	options := &slog.HandlerOptions{Level: level}
	var handler slog.Handler
	if configuration.Format == "json" {
		handler = slog.NewJSONHandler(os.Stderr, options)
	} else {
		handler = slog.NewTextHandler(os.Stderr, options)
	}
	slog.SetDefault(slog.New(handler))
}
