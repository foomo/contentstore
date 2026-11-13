package contentstore

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

const (
	// scheduleLease is how long a runner holds the runner lease without renewing it. It
	// renews it before every action, so only an action taking longer can lose it.
	scheduleLease = 2 * time.Minute
	// maxRunActions caps the actions one run executes; the rest wait for the next run.
	maxRunActions = 1000
)

// RunDueActions runs the scheduled actions due at now, oldest first (ties by ID), through
// Publish, Unpublish and Archive, so they validate, guard and notify exactly like the
// manual operations. Call it periodically, e.g. every minute, from every replica of the
// application: a run first takes the single runner lease, and while another run holds it
// the call returns at once with Busy set. One runner at a time keeps each item's actions
// in order.
//
// An action that succeeds is marked succeeded. One that fails for a reason other than an
// internal error (e.g. a required field is missing, or the item no longer exists) is
// marked failed with the error. One that fails with an internal error returns to pending
// with the error, and the item's later actions wait too, so the next run retries them in
// order.
//
// Actions run at least once: if the application stops while an action runs, the next run
// repeats it. Publish, Unpublish and Archive are safe to repeat.
//
// It returns an error only when the run cannot start, a store error stops it, it loses the
// lease, or ctx ends; the run then reports the actions processed so far.
func (e *Engine) RunDueActions(ctx context.Context, now time.Time) (*ScheduleRun, error) {
	if err := e.requireSchedules(); err != nil {
		return nil, err
	}
	holder, err := newRunnerHolder()
	if err != nil {
		return nil, internalErr(err, "schedule run failed")
	}
	acquired, err := e.schedules.AcquireScheduleRunner(ctx, holder, time.Now().UTC().Add(scheduleLease))
	if err != nil {
		return nil, internalErr(err, "acquire schedule runner failed")
	}
	if !acquired {
		return &ScheduleRun{Busy: true, Actions: []ScheduledAction{}}, nil
	}
	defer func() { _ = e.schedules.ReleaseScheduleRunner(context.WithoutCancel(ctx), holder) }()

	// Times are kept to the minute, so the actions due at now are those before the next one.
	due := now.UTC().Truncate(time.Minute).Add(time.Minute)
	actions, err := e.schedules.FindScheduledActions(ctx, ScheduledActionQuery{
		// A running action seen here was claimed by a run that ended before recording it.
		Statuses: []ScheduledActionStatus{ActionPending, ActionRunning},
		To:       &due,
		Limit:    maxRunActions,
	})
	if err != nil {
		return nil, internalErr(err, "find due scheduled actions failed")
	}

	run := &ScheduleRun{Actions: make([]ScheduledAction, 0, len(actions))}
	blocked := map[string]bool{} // items whose earlier action is pending again
	for _, action := range actions {
		if err := ctx.Err(); err != nil {
			return run, err
		}
		if blocked[action.ItemID] {
			continue
		}
		renewed, err := e.schedules.AcquireScheduleRunner(ctx, holder, time.Now().UTC().Add(scheduleLease))
		if err != nil {
			return run, internalErr(err, "renew schedule runner failed")
		}
		if !renewed {
			return run, conflict("schedule runner lease expired and another run took over")
		}
		done, err := e.runAction(ctx, action)
		if err != nil {
			return run, err
		}
		if done == nil { // cancelled meanwhile
			continue
		}
		if done.Status == ActionPending {
			blocked[done.ItemID] = true
		}
		run.Actions = append(run.Actions, *done)
	}
	return run, nil
}

// runAction claims a due action, executes it and records the outcome. It returns the
// recorded action, or nil when the action was cancelled before it could be claimed.
func (e *Engine) runAction(ctx context.Context, action *ScheduledAction) (*ScheduledAction, error) {
	claimed := *action
	claimed.Status = ActionRunning
	claimed.UpdatedAt = time.Now().UTC()
	if err := e.schedules.UpdateScheduledAction(ctx, &claimed, ActionPending, ActionRunning); err != nil {
		if errors.Is(err, ErrConflict) {
			return nil, nil
		}
		return nil, internalErr(err, "claim scheduled action failed")
	}

	failure := e.executeAction(ctx, &claimed)
	done := claimed
	now := time.Now().UTC()
	done.UpdatedAt = now
	done.Error = ""
	typed, _ := errors.AsType[*Error](failure)
	switch {
	case failure == nil:
		done.Status = ActionSucceeded
		done.ExecutedAt = &now
	case typed != nil && typed.Kind != KindInternal:
		done.Status = ActionFailed
		done.Error = failure.Error()
		done.ExecutedAt = &now
	default:
		done.Status = ActionPending
		done.Error = failure.Error()
	}
	if err := e.schedules.UpdateScheduledAction(ctx, &done, ActionRunning); err != nil && !errors.Is(err, ErrConflict) {
		// The action stays running and the next run repeats it.
		return nil, internalErr(err, "record scheduled action failed")
	}
	// A conflict means the action was deleted while it ran (its item was deleted, or the
	// content replaced); the outcome is still reported.
	return &done, nil
}

// executeAction applies the action to its item.
func (e *Engine) executeAction(ctx context.Context, action *ScheduledAction) error {
	match := ContentItemMatch{ID: action.ItemID}
	var err error
	switch action.Action {
	case ActionPublish:
		_, err = e.Publish(ctx, match)
	case ActionUnpublish:
		_, err = e.Unpublish(ctx, match)
	case ActionArchive:
		_, err = e.Archive(ctx, match)
	default:
		err = badRequest(fmt.Sprintf("unknown scheduled action %q", action.Action))
	}
	return err
}

// newRunnerHolder returns a random token identifying one run's lease.
func newRunnerHolder() (string, error) {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
