package proxy

import (
	"fmt"
	"log"
	"sync"
	"time"
)

type Logger struct {
	name string
	mu   sync.Mutex
}

func NewLogger(name string) *Logger {
	return &Logger{name: name}
}

func (l *Logger) log(level, msg string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	timestamp := time.Now().Format("2006-01-02 15:04:05")
	log.Printf("[%s] [%s] [%s] %s", timestamp, level, l.name, msg)
}

func (l *Logger) Debug(msg string) {
	l.log("DEBUG", msg)
}

func (l *Logger) Info(msg string) {
	l.log("INFO", msg)
}

func (l *Logger) Warn(msg string) {
	l.log("WARN", msg)
}

func (l *Logger) Error(msg string) {
	l.log("ERROR", msg)
}

func (l *Logger) Warnf(format string, args ...interface{}) {
	l.Warn(fmt.Sprintf(format, args...))
}

func (l *Logger) Infof(format string, args ...interface{}) {
	l.Info(fmt.Sprintf(format, args...))
}

func (l *Logger) Errorf(format string, args ...interface{}) {
	l.Error(fmt.Sprintf(format, args...))
}