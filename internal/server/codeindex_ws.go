package server

import (
	"context"
	"errors"
	"time"

	"gogen/internal/codeindex"
)

// reindexTimeout bounds one reindex. A hung embedding server must not hold
// the shared index lock until the process restarts.
const reindexTimeout = 10 * time.Minute

// wsHandleReindexCode starts the same rebuild search_meaning uses. A second
// request while one is running returns busy and does not walk again. The
// walk keeps going if the settings page closes; closing the connection (or
// the timeout) cancels it.
func wsHandleReindexCode(req *wsRequest) {
	s, ws := req.server, req.conn
	if s == nil || s.ws == nil || !s.ws.GetCodeIndexEnabled() {
		writeNoticeError(ws, "settings", "Error: code index is off")
		return
	}
	var run func(context.Context) error
	if req.target != nil && req.target.agent != nil {
		run = req.target.agent.ReindexCode
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), reindexTimeout)
		defer cancel()
		if ws.done != nil {
			go func() {
				select {
				case <-ws.done:
					cancel()
				case <-ctx.Done():
				}
			}()
		}
		var agentErr error
		ran := false
		if run != nil {
			agentErr = run(ctx)
			ran = true
		} else if s.registry != nil {
			for _, rt := range s.registry.runtimes() {
				if rt != nil && rt.agent != nil {
					agentErr = rt.agent.ReindexCode(ctx)
					ran = true
					break
				}
			}
		}
		if !ran {
			writeNoticeError(ws, "settings", "Error: code index is unavailable")
			return
		}
		if agentErr == nil {
			return
		}
		if errors.Is(agentErr, codeindex.ErrBusy) {
			writeNoticeError(ws, "settings", "Error: a code index rebuild is already running")
			return
		}
		writeNoticeError(ws, "settings", "Error: "+agentErr.Error())
	}()
}
