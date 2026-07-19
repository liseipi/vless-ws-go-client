package main

import (
	"fmt"
	"os"
	"sync"
	"time"
)

type LogLevel int

const (
	LvlDebug LogLevel = iota
	LvlInfo
	LvlWarn
	LvlError
)

var levelNames = map[string]LogLevel{
	"debug": LvlDebug,
	"info":  LvlInfo,
	"warn":  LvlWarn,
	"error": LvlError,
}

type Logger struct {
	level LogLevel
	mu    sync.Mutex
}

func NewLogger(levelStr string) *Logger {
	lvl, ok := levelNames[levelStr]
	if !ok {
		lvl = LvlInfo
	}
	return &Logger{level: lvl}
}

func (l *Logger) ts() string {
	return time.Now().Format(time.RFC3339)
}

func (l *Logger) log(lvl LogLevel, color, tag string, a ...any) {
	if lvl < l.level {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	prefix := fmt.Sprintf("\x1b[%sm[%s %s]\x1b[0m", color, tag, l.ts())
	fmt.Fprintln(os.Stdout, append([]any{prefix}, a...)...)
}

func (l *Logger) Debug(a ...any) { l.log(LvlDebug, "90", "DBG", a...) }
func (l *Logger) Info(a ...any)  { l.log(LvlInfo, "36", "INF", a...) }
func (l *Logger) Warn(a ...any)  { l.log(LvlWarn, "33", "WRN", a...) }
func (l *Logger) Error(a ...any) { l.log(LvlError, "31", "ERR", a...) }
