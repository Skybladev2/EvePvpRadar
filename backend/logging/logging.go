package logging

import (
	"log"
	"os"
	"strings"
)

var isDebug bool

func init() {
	level := strings.ToUpper(strings.TrimSpace(os.Getenv("LOG_LEVEL")))
	if level == "" {
		level = "INFO"
	}
	isDebug = level == "DEBUG"
}

// Debugf logs at DEBUG level. Only outputs when LOG_LEVEL=DEBUG.
func Debugf(format string, args ...interface{}) {
	if isDebug {
		log.Printf(format, args...)
	}
}

// Level returns the current log level ("DEBUG" or "INFO").
func Level() string {
	if isDebug {
		return "DEBUG"
	}
	return "INFO"
}
