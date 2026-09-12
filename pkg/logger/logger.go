package logger

import (
	"fmt"
	"os"
	"time"
)

type Logger struct {
	debug bool
}

func New(debug bool) *Logger { return &Logger{debug: debug} }

// clockFmt is the HH:MM:SS stamp used by every log line (Sonar: literal).
const clockFmt = "15:04:05"

func (l *Logger) Info(msg string, args ...any) {
	fmt.Fprintf(os.Stderr, "[%s] INFO: %s\n", time.Now().Format(clockFmt), fmt.Sprintf(msg, args...))
}

func (l *Logger) Warn(msg string, args ...any) {
	fmt.Fprintf(os.Stderr, "[%s] WARN: %s\n", time.Now().Format(clockFmt), fmt.Sprintf(msg, args...))
}

func (l *Logger) Error(msg string, args ...any) {
	fmt.Fprintf(os.Stderr, "[%s] ERROR: %s\n", time.Now().Format(clockFmt), fmt.Sprintf(msg, args...))
}

func (l *Logger) Debug(msg string, args ...any) {
	if l.debug {
		fmt.Fprintf(os.Stderr, "[%s] DEBUG: %s\n", time.Now().Format("15:04:05"), fmt.Sprintf(msg, args...))
	}
}
