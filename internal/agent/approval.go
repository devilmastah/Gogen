package agent

import (
	"context"
	"errors"
)

// DeleteRequest describes a file deletion awaiting user approval.
type DeleteRequest struct {
	Paths  []string
	Reason string
}

// DeleteApprover decides whether a delete operation may proceed.
type DeleteApprover func(ctx context.Context, req DeleteRequest) (approved bool, err error)

type deleteApproverKey struct{}
type deleteApprovalPolicyKey struct{}

// ContextWithDeleteApprover attaches a delete approval handler to ctx.
func ContextWithDeleteApprover(ctx context.Context, approver DeleteApprover) context.Context {
	if approver == nil {
		return ctx
	}
	return context.WithValue(ctx, deleteApproverKey{}, approver)
}

// DeleteApproverFromContext returns the delete approver from ctx, if any.
func DeleteApproverFromContext(ctx context.Context) DeleteApprover {
	if ctx == nil {
		return nil
	}
	approver, _ := ctx.Value(deleteApproverKey{}).(DeleteApprover)
	return approver
}

// ContextWithDeleteApprovalRequired sets whether deletes require approval in this ctx.
func ContextWithDeleteApprovalRequired(ctx context.Context, required bool) context.Context {
	return context.WithValue(ctx, deleteApprovalPolicyKey{}, required)
}

func deleteApprovalRequired(ctx context.Context) bool {
	if ctx == nil {
		return true
	}
	required, ok := ctx.Value(deleteApprovalPolicyKey{}).(bool)
	if ok {
		return required
	}
	return true
}

func (e *Executor) deleteApprovalRequired(ctx context.Context) bool {
	if !e.DeleteApprovalRequired() {
		return false
	}
	return deleteApprovalRequired(ctx)
}

func (e *Executor) requireDeleteApproval(ctx context.Context, paths []string, reason string) error {
	if len(paths) == 0 || !e.deleteApprovalRequired(ctx) {
		return nil
	}
	approver := DeleteApproverFromContext(ctx)
	if approver == nil {
		return ErrDeleteApprovalRequired
	}
	ok, err := approver(ctx, DeleteRequest{Paths: paths, Reason: reason})
	if err != nil {
		return err
	}
	if !ok {
		return ErrDeleteDenied
	}
	return nil
}

// CommandRequest describes a shell command that matched a blocked-command
// pattern and is awaiting user approval.
type CommandRequest struct {
	Command string
	Reason  string // the safety pattern that matched
}

// CommandApprover decides whether a blocked command may proceed.
type CommandApprover func(ctx context.Context, req CommandRequest) (approved bool, err error)

type commandApproverKey struct{}

// ContextWithCommandApprover attaches a command approval handler to ctx.
func ContextWithCommandApprover(ctx context.Context, approver CommandApprover) context.Context {
	if approver == nil {
		return ctx
	}
	return context.WithValue(ctx, commandApproverKey{}, approver)
}

// CommandApproverFromContext returns the command approver from ctx, if any.
func CommandApproverFromContext(ctx context.Context) CommandApprover {
	if ctx == nil {
		return nil
	}
	approver, _ := ctx.Value(commandApproverKey{}).(CommandApprover)
	return approver
}

// ErrCommandDenied is returned when the user declines a blocked command.
var ErrCommandDenied = errors.New("command denied by user")

// requireCommandApproval asks the user to allow a command that matched a
// blocked-command pattern. With no approver attached (headless/automation
// contexts) it fails closed with the original block error, so a blocked
// command is never executed unattended.
func (e *Executor) requireCommandApproval(ctx context.Context, command, pattern string) error {
	approver := CommandApproverFromContext(ctx)
	if approver == nil {
		return &CommandBlockedError{Pattern: pattern}
	}
	ok, err := approver(ctx, CommandRequest{Command: command, Reason: pattern})
	if err != nil {
		return err
	}
	if !ok {
		return ErrCommandDenied
	}
	return nil
}
