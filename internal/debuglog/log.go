package debuglog

import (
	"encoding/json"
	"os"
	"sync"
	"testing"
	"time"
)

var (
	cfgMu      sync.RWMutex
	cfgLog     string
	cfgLogSet  bool
	cfgSess    string
	cfgSessSet bool

	writeMu       sync.Mutex
	logFile       *os.File
	logPathCached string
)

// Configure applies runtime debug log settings from merged config.
func Configure(logPath, sessionID string) {
	cfgMu.Lock()
	cfgLog = logPath
	cfgLogSet = true
	cfgSess = sessionID
	cfgSessSet = true
	cfgMu.Unlock()

	// Close open files without holding cfgMu (lock order: never nest writeMu under cfgMu).
	writeMu.Lock()
	defer writeMu.Unlock()
	if logFile != nil {
		_ = logFile.Close()
		logFile = nil
	}
	logPathCached = ""
	if responseFile != nil {
		_ = responseFile.Close()
		responseFile = nil
	}
	responsePathCached = ""
}

func logPath() string {
	cfgMu.RLock()
	if cfgLogSet {
		p := cfgLog
		cfgMu.RUnlock()
		return p
	}
	cfgMu.RUnlock()
	// Unit tests often inherit a developer's GOGEN_DEBUG_LOG and would
	// otherwise pollute the real session log with fixture entries.
	if testing.Testing() {
		return ""
	}
	return os.Getenv("GOGEN_DEBUG_LOG")
}

func sessionID() string {
	cfgMu.RLock()
	if cfgSessSet {
		s := cfgSess
		cfgMu.RUnlock()
		return s
	}
	cfgMu.RUnlock()
	if testing.Testing() {
		return ""
	}
	return os.Getenv("GOGEN_DEBUG_SESSION")
}

// Enabled reports whether Write would emit an entry — i.e. a debug log path is
// configured. Callers use it to skip diagnostic accounting that exists only to
// be logged, so the measurement costs nothing when debugging is off.
func Enabled() bool {
	return logPath() != ""
}

// Write appends a JSON debug entry when debug logging is configured.
func Write(location, message, hypothesisID string, data map[string]any) {
	logPath := logPath()
	if logPath == "" {
		return
	}
	entry := map[string]any{
		"timestamp":    time.Now().UnixMilli(),
		"location":     location,
		"message":      message,
		"hypothesisId": hypothesisID,
		"data":         data,
	}
	if sid := sessionID(); sid != "" {
		entry["sessionId"] = sid
	}
	b, err := json.Marshal(entry)
	if err != nil {
		return
	}
	line := append(b, '\n')

	writeMu.Lock()
	defer writeMu.Unlock()
	if logPath != logPathCached {
		if logFile != nil {
			_ = logFile.Close()
			logFile = nil
		}
		logPathCached = logPath
	}
	if logFile == nil {
		// 0600: the log carries prompts, tool arguments and reasoning.
		f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return
		}
		logFile = f
	}
	_, _ = logFile.Write(line)
}
