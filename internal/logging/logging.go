package logging

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"sync"
	"time"
)

type Level int

const (
	LevelDebug Level = iota
	LevelInfo
	LevelWarn
	LevelError
)

func parseLevel(s string) Level {
	switch strings.ToLower(s) {
	case "debug":
		return LevelDebug
	case "warn", "warning":
		return LevelWarn
	case "error":
		return LevelError
	default:
		return LevelInfo
	}
}

type Logger struct {
	mu    sync.Mutex
	out   io.Writer
	level Level
	std   *log.Logger
}

var defaultLogger = New(os.Stdout, "info")

func New(out io.Writer, level string) *Logger {
	return &Logger{
		out:   out,
		level: parseLevel(level),
		std:   log.New(out, "", 0),
	}
}

func Default() *Logger { return defaultLogger }

func SetDefault(l *Logger) { defaultLogger = l }

func (l *Logger) log(lvl Level, msg string, fields map[string]any) {
	if lvl < l.level {
		return
	}
	rec := map[string]any{
		"time":  time.Now().UTC().Format(time.RFC3339Nano),
		"level": levelString(lvl),
		"msg":   msg,
	}
	for k, v := range fields {
		rec[k] = v
	}
	b, err := json.Marshal(rec)
	if err != nil {
		b = []byte(fmt.Sprintf(`{"level":"error","msg":"failed to marshal log: %v"}`, err))
	}
	l.mu.Lock()
	l.std.Output(2, string(b))
	l.mu.Unlock()
}

func levelString(l Level) string {
	switch l {
	case LevelDebug:
		return "debug"
	case LevelInfo:
		return "info"
	case LevelWarn:
		return "warn"
	case LevelError:
		return "error"
	}
	return "info"
}

func (l *Logger) Debug(msg string, fields map[string]any) { l.log(LevelDebug, msg, fields) }
func (l *Logger) Info(msg string, fields map[string]any)  { l.log(LevelInfo, msg, fields) }
func (l *Logger) Warn(msg string, fields map[string]any)  { l.log(LevelWarn, msg, fields) }
func (l *Logger) Error(msg string, fields map[string]any) { l.log(LevelError, msg, fields) }

func F(args ...any) map[string]any {
	m := make(map[string]any, len(args)/2)
	for i := 0; i+1 < len(args); i += 2 {
		k, ok := args[i].(string)
		if !ok {
			continue
		}
		m[k] = args[i+1]
	}
	return m
}
