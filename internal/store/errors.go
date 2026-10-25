package store

import (
	"errors"
	"fmt"

	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
)

// Errors of the record. Every write refusal of spec §5.2 (the version moved, the terminal rule, PrevVersion
// 0, a fenced dispatch) is, or wraps, taskstore.ErrConcurrentModification; a task outside the caller's
// scope, unknown or malformed is a2a.ErrTaskNotFound (spec §4).
var (
	// ErrFenced is a write by a dispatch whose fence no longer holds: its attempt is not the task's latest,
	// has ended, or carries a newer dispatch_id (spec §6.4, D77). It wraps ErrConcurrentModification, so
	// a2a-go reloads the task and ends the execution (a terminal task) or stops it as a parallel active
	// execution; either way the fenced dispatch writes nothing more.
	ErrFenced = fmt.Errorf("store: dispatch fenced by a newer dispatch of the task: %w", taskstore.ErrConcurrentModification)

	// ErrSynthesizedFailure is a failed status a2a-go synthesized on its own error path (SetTaskFailed: a
	// failed status with no message) while the task's dispatch is registered on this replica. The relay
	// returns its outcome from the registry instead, and the workflow writes the terminal event (D8).
	ErrSynthesizedFailure = errors.New("store: refused a failed status the relay did not emit (a2a-go SetTaskFailed)")

	// ErrAttemptNotFound is an attempt (task, n) that does not exist.
	ErrAttemptNotFound = errors.New("store: attempt not found")

	// ErrAttemptNotOpen is a dispatch that cannot start on attempt n: n is not the task's latest attempt,
	// or it has ended (spec §4, §6.4).
	ErrAttemptNotOpen = errors.New("store: attempt is not the task's open latest attempt")

	// ErrUpstreamIDMismatch is a first upstream task id that differs from the one already recorded for the
	// attempt (spec §4): the upstream named another task.
	ErrUpstreamIDMismatch = errors.New("store: the attempt already records a different first upstream task id")

	// ErrWorkItemNotFound is a work item that does not exist or belongs to another task; the two are not
	// told apart (spec §14.1, D66).
	ErrWorkItemNotFound = errors.New("store: work item not found for this task")

	// ErrWorkItemConsumed is a work item already consumed by an earlier dispatch or review message.
	ErrWorkItemConsumed = errors.New("store: work item already consumed")
)
