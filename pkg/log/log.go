package log

import (
	"fmt"
	golog "log"
	"os"
	"sync"
)

var (
	debug  bool
	logger = golog.New(os.Stderr, "", golog.LstdFlags|golog.Lshortfile)
	mu     sync.Mutex
)

func SetDebug(enabled bool) {
	mu.Lock()
	defer mu.Unlock()
	debug = enabled
}

func IsDebug() bool {
	mu.Lock()
	defer mu.Unlock()
	return debug
}





func Debugf(format string, args ...interface{}) {
	if !IsDebug() {
		return
	}
	_ = logger.Output(2, fmt.Sprintf(format, args...))
}
