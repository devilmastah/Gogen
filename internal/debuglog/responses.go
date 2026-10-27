package debuglog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var (
	responsePathCached string
	responseFile       *os.File
)

// LLMResponseRecord is a full snapshot of one model turn for offline review.
type LLMResponseRecord struct {
	Timestamp      int64                  `json:"timestamp"`
	SessionID      string                 `json:"sessionId,omitempty"`
	Model          string                 `json:"model"`
	Source         string                 `json:"source"` // stream, stream-fallback, non-stream
	Attempt        int                    `json:"attempt,omitempty"`
	FinishReason   string                 `json:"finishReason,omitempty"`
	Error          string                 `json:"error,omitempty"`
	Content        string                 `json:"content"`
	Refusal        string                 `json:"refusal,omitempty"`
	Reasoning      string                 `json:"reasoning,omitempty"`
	ExtraFields    map[string]string      `json:"extraFields,omitempty"`
	DeltaSamples   []string               `json:"deltaSamples,omitempty"`
	ChunkDeltas    []string               `json:"chunkDeltas,omitempty"`
	DisplayContent string                 `json:"displayContent,omitempty"`
	ToolCalls      []LLMToolCallRecord    `json:"toolCalls,omitempty"`
	DroppedTools   []LLMDroppedToolRecord `json:"droppedTools,omitempty"`
	PartialTools   []LLMDroppedToolRecord `json:"partialTools,omitempty"`
	ChunkCount     int                    `json:"chunkCount,omitempty"`
	UsedFallback   bool                   `json:"usedFallback,omitempty"`
	Usage          map[string]int         `json:"usage,omitempty"`
}

type LLMToolCallRecord struct {
	Index    int            `json:"index,omitempty"`
	ID       string         `json:"id,omitempty"`
	Name     string         `json:"name"`
	Args     map[string]any `json:"args,omitempty"`
	ArgsJSON string         `json:"argsJson,omitempty"`
}

type LLMDroppedToolRecord struct {
	Index    int    `json:"index,omitempty"`
	ID       string `json:"id,omitempty"`
	Name     string `json:"name,omitempty"`
	ArgsJSON string `json:"argsJson,omitempty"`
}

func deriveResponseLogPath(debugPath, sessionID string) string {
	if env := os.Getenv("GOGEN_RESPONSE_LOG"); env != "" {
		return env
	}
	if debugPath == "" {
		return ""
	}
	dir := filepath.Dir(debugPath)
	base := filepath.Base(debugPath)
	name := "llm-responses.jsonl"
	if sessionID != "" {
		name = "llm-responses-" + sessionID + ".jsonl"
	} else if strings.HasPrefix(base, "debug-") && strings.HasSuffix(base, ".log") {
		suffix := strings.TrimSuffix(strings.TrimPrefix(base, "debug-"), ".log")
		if suffix != "" {
			name = "llm-responses-" + suffix + ".jsonl"
		}
	}
	return filepath.ToSlash(filepath.Join(dir, name))
}

func responseLogPath() string {
	return deriveResponseLogPath(logPath(), sessionID())
}

// WriteLLMResponse appends a full model response record when response logging is enabled.
func WriteLLMResponse(rec LLMResponseRecord) {
	path := responseLogPath()
	if path == "" {
		return
	}
	if rec.Timestamp == 0 {
		rec.Timestamp = time.Now().UnixMilli()
	}
	if rec.SessionID == "" {
		rec.SessionID = sessionID()
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return
	}
	line := append(b, '\n')

	writeMu.Lock()
	defer writeMu.Unlock()
	if path != responsePathCached {
		if responseFile != nil {
			_ = responseFile.Close()
			responseFile = nil
		}
		responsePathCached = path
	}
	if responseFile == nil {
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			return
		}
		// 0600: the response log carries full prompts, reasoning and tool
		// arguments.
		f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return
		}
		responseFile = f
	}
	_, _ = responseFile.Write(line)
}
