package contentstore

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"
)

const (
	// scheduleLease is how long a runner holds the runner lease without renewing it. It
	// renews it before every action and every leaseRenewal while one executes.
	scheduleLease = 2 * time.Minute
	// maxRunActions caps the actions one run executes; the rest wait for the next run.
	maxRunActions = 1000
)

// leaseRenewal is how often a runner renews its lease while an action executes. Tests
// shorten it.
var leaseRenewal = scheduleLease / 4

// errLeaseLost ends an executing action's context when its run loses the runner lease.
var errLeaseLost = errors.New("contentstore: schedule runner lease lost")

// ScheduleExecutor runs one due scheduled action in place of the built-in Publish,
// Unpublish or Archive of its item, e.g. to apply an action scheduled on a tree container
// to the items in it as they are when it runs. builtin runs the built-in operation, so an
// executor can pass on the actions it does not handle. The error decides the outcome as a
// built-in operation's does: nil succeeds, an *Error of a kind other than KindInternal
// fails the action with its message, and any other error returns the action to pending
// for the next run, so an executor must be safe to repeat. The runner renews its lease
// while the executor runs; ctx ends when the lease is lost anyway.
type ScheduleExecutor func(ctx context.Context, action ScheduledAction, builtin func(context.Context) error) error

// RunDueActions runs the scheduled actions due at now, oldest first (ties by ID), through
// Publish, Unpublish and Archive, so they validate, guard and notify exactly like the
// manual operations, or through the Deps.ScheduleExecutor when one is set. Call it
// periodically, e.g. every minute, from every replica of the application: a run first
// takes the single runner lease, and while another run holds it the call returns at once
// with Busy set. One runner at a time keeps each item's actions in order.
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
		done, err := e.runAction(ctx, holder, action)
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

// runAction claims a due action, executes it while holding the lease of the run
// identified by holder, and records the outcome. It returns the recorded action, or nil
// when the action was cancelled before it could be claimed.
func (e *Engine) runAction(ctx context.Context, holder string, action *ScheduledAction) (*ScheduledAction, error) {
	claimed := *action
	claimed.Status = ActionRunning
	claimed.UpdatedAt = time.Now().UTC()
	if err := e.schedules.UpdateScheduledAction(ctx, &claimed, ActionPending, ActionRunning); err != nil {
		if errors.Is(err, ErrConflict) {
			return nil, nil
		}
		return nil, internalErr(err, "claim scheduled action failed")
	}

	actionCtx, release := e.holdLease(ctx, holder)
	failure := e.executeScheduled(actionCtx, &claimed)
	if release() {
		// Another run may have taken the action over; it records the outcome, and the
		// action stays running until then.
		return nil, conflict("schedule runner lease expired and another run took over")
	}
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

// holdLease renews the lease of the run identified by holder in the background while one
// action executes, so an action may take longer than the lease. The returned context ends
// when the lease is lost: another run holds it, or renewing has failed for half the lease.
// release stops the renewal and reports whether the lease was lost.
func (e *Engine) holdLease(ctx context.Context, holder string) (context.Context, func() (lost bool)) {
	ctx, cancel := context.WithCancelCause(ctx)
	stop := make(chan struct{})
	var renewing sync.WaitGroup
	renewing.Go(func() {
		ticker := time.NewTicker(leaseRenewal)
		defer ticker.Stop()
		renewed := time.Now()
		for {
			select {
			case <-stop:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			held, err := e.schedules.AcquireScheduleRunner(ctx, holder, time.Now().UTC().Add(scheduleLease))
			switch {
			case err == nil && held:
				renewed = time.Now()
			case err == nil || time.Since(renewed) >= scheduleLease/2:
				cancel(errLeaseLost)
				return
			}
		}
	})
	return ctx, func() bool {
		close(stop)
		renewing.Wait()
		lost := errors.Is(context.Cause(ctx), errLeaseLost)
		cancel(nil)
		return lost
	}
}

// executeScheduled runs an action through the application's executor, or the built-in
// operation when there is none.
func (e *Engine) executeScheduled(ctx context.Context, action *ScheduledAction) error {
	builtin := func(ctx context.Context) error { return e.executeAction(ctx, action) }
	if e.execute == nil {
		return builtin(ctx)
	}
	return e.execute(ctx, *action, builtin)
}

// executeAction applies the built-in operation of the action to its item.
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
