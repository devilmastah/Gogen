package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"time"

	"gogen/internal/llm"
)

// deltaFile holds messages appended since the last full snapshot save.
type deltaFile struct {
	Messages    []llm.Message `json:"messages"`
	TokenCounts []int         `json:"tokenCounts,omitempty"`
	// BaseCount is the number of messages in the full snapshot this delta
	// was written against (the message index where Messages starts). Load
	// uses it to tell "snapshot is the delta's base" (merge) from "snapshot
	// already contains the delta" (stale) and "snapshot was truncated after
	// the delta was written" (drop). Zero means a legacy delta predating
	// this field; legacy deltas are merged unconditionally.
	BaseCount int `json:"baseCount,omitempty"`
}

func (s *Store) deltaPath(workingDir, id string) string {
	if s.globalDir != "" {
		return filepath.Join(s.globalDir, id+".delta")
	}
	return filepath.Join(s.dir(workingDir), id+".delta")
}

// decodeDeltaFile decodes raw delta-file bytes. Callers capture the bytes
// under the store lock (LoadInWorkingDir's first critical section) and decode
// them at the merge step, so the merge decisions stay paired with the file
// state they were based on.
func decodeDeltaFile(data []byte) (deltaFile, error) {
	var df deltaFile
	if err := json.Unmarshal(data, &df); err != nil {
		return deltaFile{}, err
	}
	return df, nil
}

// deltaPrefixMatches reports whether the snapshot's trailing messages (the
// portion beyond the delta's base) are the same messages as the delta's
// leading messages — i.e., the snapshot already absorbed a prefix of the
// delta (a concurrent full save raced the delta; see LoadInWorkingDir's
// mergeFrom branch). Only the persisted content is compared: transient
// fields that are not round-tripped identically (the stream Index and
// ArgsStabilized) are ignored.
func deltaPrefixMatches(snapshotTail, deltaPrefix []llm.Message) bool {
	if len(snapshotTail) != len(deltaPrefix) {
		return false
	}
	for i := range snapshotTail {
		if !sameMessageContent(snapshotTail[i], deltaPrefix[i]) {
			return false
		}
	}
	return true
}

// sameMessageContent compares two messages by their persisted fields. Used
// by deltaPrefixMatches to decide whether a snapshot absorbed a delta's
// prefix; tool-call Args maps are compared structurally because their JSON
// round-trip order can differ.
func sameMessageContent(a, b llm.Message) bool {
	if a.Role != b.Role || a.Content != b.Content ||
		a.Reasoning != b.Reasoning || a.Refusal != b.Refusal ||
		a.ToolCallID != b.ToolCallID || a.Model != b.Model {
		return false
	}
	if !a.CreatedAt.Equal(b.CreatedAt) {
		return false
	}
	// Images (vision input) are persisted with json:"images,omitempty"; the
	// element-wise compare keeps the persisted-content contract complete.
	if len(a.Images) != len(b.Images) {
		return false
	}
	for i := range a.Images {
		if a.Images[i] != b.Images[i] {
			return false
		}
	}
	if len(a.ToolCalls) != len(b.ToolCalls) {
		return false
	}
	for i := range a.ToolCalls {
		ta, tb := a.ToolCalls[i], b.ToolCalls[i]
		if ta.ID != tb.ID || ta.Name != tb.Name || ta.ArgsStr != tb.ArgsStr {
			return false
		}
		if !reflect.DeepEqual(ta.Args, tb.Args) {
			return false
		}
	}
	return true
}

// sessionUpdatedAt returns the effective persisted update time for a session
// file: the file's updated field, or the delta file's mtime when that is
// newer. Incremental saves (AppendMessages) and TouchSession update the
// metadata index and the file mtimes but deliberately do NOT rewrite the
// session file's updated field, so the no-index fallback scans must consult
// the delta's mtime to keep delta-only updates in their recency order.
// Correct in both directions: after a full save that superseded a delta, the
// file's updated is newer than the (surviving, crash-left) delta's mtime, so
// the max picks the file field. Callers must hold s.mu.
func (s *Store) sessionUpdatedAt(workingDir, id string, fileUpdated time.Time) time.Time {
	if fi, err := os.Stat(s.deltaPath(workingDir, id)); err == nil {
		if mt := fi.ModTime().UTC(); mt.After(fileUpdated) {
			return mt
		}
	}
	return fileUpdated
}

// clearDeltaFile removes the delta file for a session.
func (s *Store) clearDeltaFile(workingDir, id string) error {
	return os.Remove(s.deltaPath(workingDir, id))
}
