package telemetry

import (
	"log/slog"
	"os"
)

func NewLogger() *slog.Logger {
	level := slog.LevelInfo
	if os.Getenv("PWRAP_DEBUG") == "1" {
		level = slog.LevelDebug
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
}
