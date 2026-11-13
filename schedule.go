package contentstore

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// ContentSchedules is the scheduled-action API of the engine. *Engine implements it. It is
// separate from ContentStore so existing implementations of that interface keep compiling.
//
// A scheduled action publishes, unpublishes or archives one item at a given minute. The
// engine stores the actions and runs the due ones when RunDueActions is called; the
// application calls it periodically, e.g. every minute.
type ContentSchedules interface {
	ScheduleAction(ctx context.Context, input ScheduleActionInput) (*ScheduledAction, error)
	CancelScheduledAction(ctx context.Context, id string) (bool, error)
	ListScheduledActions(ctx context.Context, input ListScheduledActionsInput) (*ScheduledActionPage, error)
	RunDueActions(ctx context.Context, now time.Time) (*ScheduleRun, error)
}

var _ ContentSchedules = (*Engine)(nil)

func (t ScheduledActionType) valid() bool {
	return t == ActionPublish || t == ActionUnpublish || t == ActionArchive
}

func (s ScheduledActionStatus) valid() bool {
	return s == ActionPending || s == ActionRunning || s == ActionSucceeded || s == ActionFailed
}

// requireSchedules rejects scheduled-action calls on an engine whose store keeps none.
func (e *Engine) requireSchedules() error {
	if e.schedules == nil {
		return internalErr(nil, "store does not support scheduled actions")
	}
	return nil
}

// ScheduleAction schedules a publish, unpublish or archive of an item. The time is kept to
// the minute and must lie in the future, and an item takes at most one action per minute:
// a second one is a conflict. Whether the action can succeed (e.g. publishing an item that
// is archived by then) is decided when it runs, since the item can change until then.
func (e *Engine) ScheduleAction(ctx context.Context, input ScheduleActionInput) (*ScheduledAction, error) {
	if !input.Action.valid() {
		return nil, badRequest(fmt.Sprintf("unknown scheduled action %q", input.Action))
	}
	if input.At.IsZero() {
		return nil, badRequest("at is required")
	}
	now := time.Now().UTC()
	at := input.At.UTC().Truncate(time.Minute)
	if !at.After(now) {
		return nil, badRequest("at must be a minute in the future")
	}
	if err := e.requireSchedules(); err != nil {
		return nil, err
	}
	item, err := e.loadByMatch(ctx, input.Match)
	if err != nil {
		return nil, err
	}
	action := &ScheduledAction{
		ID:        e.newID(),
		ItemID:    item.ID,
		ItemType:  item.Type,
		Action:    input.Action,
		At:        at,
		Status:    ActionPending,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := e.schedules.InsertScheduledAction(ctx, action); err != nil {
		if errors.Is(err, ErrConflict) {
			return nil, conflict(fmt.Sprintf("item %q already has a scheduled action at %s", item.ID, at.Format(time.RFC3339)))
		}
		return nil, internalErr(err, "schedule action failed")
	}
	return action, nil
}

// CancelScheduledAction deletes a pending action. Cancelling a missing action is
// idempotent (returns false); one that is running or has run can no longer be cancelled.
func (e *Engine) CancelScheduledAction(ctx context.Context, id string) (bool, error) {
	if id == "" {
		return false, badRequest("id is required")
	}
	if err := e.requireSchedules(); err != nil {
		return false, err
	}
	deleted, err := e.schedules.DeleteScheduledActions(ctx, ScheduledActionQuery{ID: id, Statuses: []ScheduledActionStatus{ActionPending}})
	if err != nil {
		return false, internalErr(err, "cancel scheduled action failed")
	}
	if deleted > 0 {
		return true, nil
	}
	action, err := e.schedules.GetScheduledAction(ctx, id)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, internalErr(err, "cancel scheduled action failed")
	}
	if action.Status == ActionPending {
		// A runner returned it to pending after the delete above.
		return false, conflict("scheduled action changed concurrently: retry")
	}
	return false, notAcceptable(fmt.Sprintf("scheduled action is %s and can no longer be cancelled", action.Status))
}

// ListScheduledActions returns one page of scheduled actions, ordered by time (ties by ID).
// Actions that ran are kept, so this also lists an item's schedule history.
func (e *Engine) ListScheduledActions(ctx context.Context, input ListScheduledActionsInput) (*ScheduledActionPage, error) {
	limit := input.Limit
	switch {
	case limit == 0:
		limit = defaultChildrenLimit
	case limit < 0 || limit > maxChildrenLimit:
		return nil, badRequest(fmt.Sprintf("limit must be between 1 and %d", maxChildrenLimit))
	}
	for _, status := range input.Statuses {
		if !status.valid() {
			return nil, badRequest(fmt.Sprintf("unknown scheduled action status %q", status))
		}
	}
	after, err := decodeScheduleCursor(input.Cursor)
	if err != nil {
		return nil, err
	}
	if err := e.requireSchedules(); err != nil {
		return nil, err
	}
	q := ScheduledActionQuery{Statuses: input.Statuses, From: input.From, To: input.To, After: after, Limit: limit + 1}
	if input.Match.ID != "" || input.Match.Key != "" {
		item, err := e.loadByMatch(ctx, input.Match)
		if err != nil {
			return nil, err
		}
		q.ItemID = item.ID
	}
	found, err := e.schedules.FindScheduledActions(ctx, q)
	if err != nil {
		return nil, internalErr(err, "list scheduled actions failed")
	}
	page := &ScheduledActionPage{Items: make([]ScheduledAction, 0, min(len(found), limit))}
	if len(found) > limit {
		found = found[:limit]
		last := found[limit-1]
		page.NextCursor = encodeScheduleCursor(&ScheduleCursor{At: last.At, ID: last.ID})
	}
	for _, action := range found {
		page.Items = append(page.Items, *action)
	}
	return page, nil
}

func encodeScheduleCursor(c *ScheduleCursor) string {
	b, _ := json.Marshal(c) // a ScheduleCursor always marshals
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeScheduleCursor(cursor string) (*ScheduleCursor, error) {
	if cursor == "" {
		return nil, nil
	}
	b, err := base64.RawURLEncoding.DecodeString(cursor)
	var c ScheduleCursor
	if err != nil || json.Unmarshal(b, &c) != nil || c.ID == "" || c.At.IsZero() {
		return nil, badRequest("invalid cursor")
	}
	return &c, nil
}

// dropScheduledActions deletes the actions of a deleted item. A failure is ignored: the
// item is gone either way, and the runner fails an action left behind as "item not found".
func (e *Engine) dropScheduledActions(ctx context.Context, itemID string) {
	if e.schedules == nil {
		return
	}
	_, _ = e.schedules.DeleteScheduledActions(ctx, ScheduledActionQuery{ItemID: itemID})
}
