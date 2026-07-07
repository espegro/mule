package logging

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
)

type Logger struct {
	format string
	level  slog.Level
	out    io.Writer
}

func New(format, level string) *Logger {
	lvl := slog.LevelInfo
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	}
	return &Logger{format: format, level: lvl, out: os.Stderr}
}

func (l *Logger) Debug(event string, fields ...any) { l.log(slog.LevelDebug, event, fields...) }
func (l *Logger) Info(event string, fields ...any)  { l.log(slog.LevelInfo, event, fields...) }
func (l *Logger) Warn(event string, fields ...any)  { l.log(slog.LevelWarn, event, fields...) }
func (l *Logger) Error(event string, fields ...any) { l.log(slog.LevelError, event, fields...) }

func (l *Logger) log(level slog.Level, event string, fields ...any) {
	if level < l.level {
		return
	}
	m := map[string]any{"level": strings.ToLower(level.String()), "event": event}
	for i := 0; i+1 < len(fields); i += 2 {
		key, ok := fields[i].(string)
		if !ok {
			continue
		}
		m[key] = fields[i+1]
	}
	if l.format == "json" {
		_ = json.NewEncoder(l.out).Encode(m)
		return
	}
	fmt.Fprintf(l.out, "level=%s event=%s", m["level"], event)
	for i := 0; i+1 < len(fields); i += 2 {
		key, ok := fields[i].(string)
		if !ok {
			continue
		}
		fmt.Fprintf(l.out, " %s=%v", key, fields[i+1])
	}
	fmt.Fprintln(l.out)
}
