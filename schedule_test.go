package contentstore

import (
	"context"
	"slices"
	"testing"
	"time"
)

// newScheduleEngine builds an engine with tree and SEO types over store.
func newScheduleEngine(store Store, n *recordingNotifier) *Engine {
	deps := Deps{Store: store, Schemas: treeRegistry(), Locales: testLocales, NewID: testNewID()}
	if n != nil {
		deps.Notify = n.record
	}
	return New(deps)
}

// scheduleBase is a whole minute an hour from now: actions at it and after are in the future.
func scheduleBase() time.Time {
	return time.Now().UTC().Truncate(time.Minute).Add(time.Hour)
}

// mustSchedule schedules action on the item with the given key and fails the test on error.
func mustSchedule(t *testing.T, e *Engine, key string, action ScheduledActionType, at time.Time) *ScheduledAction {
	t.Helper()
	scheduled, err := e.ScheduleAction(context.Background(), ScheduleActionInput{Match: ContentItemMatch{Key: key}, Action: action, At: at})
	if err != nil {
		t.Fatalf("schedule %s of %q at %s: %v", action, key, at, err)
	}
	return scheduled
}

func mustList(t *testing.T, e *Engine, input ListScheduledActionsInput) *ScheduledActionPage {
	t.Helper()
	page, err := e.ListScheduledActions(context.Background(), input)
	if err != nil {
		t.Fatalf("list scheduled actions: %v", err)
	}
	return page
}

func actionIDs(actions []ScheduledAction) []string {
	ids := make([]string, len(actions))
	for i, a := range actions {
		ids[i] = a.ID
	}
	return ids
}

func TestScheduleActionValidation(t *testing.T) {
	ctx := context.Background()
	e := newScheduleEngine(newFakeScheduleStore(), nil)
	seedPublishable(t, e, "k1", "o1")
	future := scheduleBase()

	tests := map[string]struct {
		input ScheduleActionInput
		kind  Kind
	}{
		"unknown action": {ScheduleActionInput{Match: ContentItemMatch{Key: "k1"}, Action: "delete", At: future}, KindBadRequest},
		"missing action": {ScheduleActionInput{Match: ContentItemMatch{Key: "k1"}, At: future}, KindBadRequest},
		"missing time":   {ScheduleActionInput{Match: ContentItemMatch{Key: "k1"}, Action: ActionPublish}, KindBadRequest},
		"past time":      {ScheduleActionInput{Match: ContentItemMatch{Key: "k1"}, Action: ActionPublish, At: time.Now().Add(-time.Hour)}, KindBadRequest},
		"current minute": {ScheduleActionInput{Match: ContentItemMatch{Key: "k1"}, Action: ActionPublish, At: time.Now()}, KindBadRequest},
		"empty match":    {ScheduleActionInput{Action: ActionPublish, At: future}, KindBadRequest},
		"missing item":   {ScheduleActionInput{Match: ContentItemMatch{Key: "missing"}, Action: ActionPublish, At: future}, KindNotFound},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := e.ScheduleAction(ctx, tt.input)
			wantKind(t, err, tt.kind)
		})
	}
}

func TestScheduleActionKeepsMinutesAndRejectsConflicts(t *testing.T) {
	ctx := context.Background()
	e := newScheduleEngine(newFakeScheduleStore(), nil)
	seedPublishable(t, e, "k1", "o1")
	seedPublishable(t, e, "k2", "o2")
	item := getItem(t, e, "id-1")
	base := scheduleBase()

	scheduled := mustSchedule(t, e, "k1", ActionPublish, base.Add(25*time.Second))
	if !scheduled.At.Equal(base) || scheduled.At.Location() != time.UTC {
		t.Fatalf("at = %v, want %v in UTC", scheduled.At, base)
	}
	if scheduled.ItemID != item.ID || scheduled.ItemType != testType || scheduled.Status != ActionPending {
		t.Fatalf("unexpected action %#v", scheduled)
	}

	// The same minute conflicts whatever the action, the seconds or how the item is matched.
	for _, input := range []ScheduleActionInput{
		{Match: ContentItemMatch{Key: "k1"}, Action: ActionUnpublish, At: base.Add(50 * time.Second)},
		{Match: ContentItemMatch{ID: item.ID}, Action: ActionPublish, At: base.In(time.FixedZone("CET", 3600))},
	} {
		_, err := e.ScheduleAction(ctx, input)
		wantKind(t, err, KindConflict)
	}

	mustSchedule(t, e, "k1", ActionUnpublish, base.Add(time.Minute))
	mustSchedule(t, e, "k2", ActionPublish, base)

	// Scheduling is not content: the item is unchanged.
	if got := getItem(t, e, item.ID); got.Draft.Revision != item.Draft.Revision || !got.UpdatedAt.Equal(item.UpdatedAt) {
		t.Fatalf("scheduling changed the item: %#v", got)
	}
}

// disabledScheduleStore is a schedule store whose schedules are turned off.
type disabledScheduleStore struct{ *fakeScheduleStore }

func (disabledScheduleStore) SchedulesEnabled() bool { return false }

func TestScheduledActionsNeedAScheduleStore(t *testing.T) {
	ctx := context.Background()
	for name, store := range map[string]Store{
		"store without schedules": newFakeStore(),
		"schedules disabled":      disabledScheduleStore{newFakeScheduleStore()},
	} {
		t.Run(name, func(t *testing.T) {
			e := newScheduleEngine(store, nil)
			seedPublishable(t, e, "k1", "o1")
			_, err := e.ScheduleAction(ctx, ScheduleActionInput{Match: ContentItemMatch{Key: "k1"}, Action: ActionPublish, At: scheduleBase()})
			wantKind(t, err, KindInternal)
			_, err = e.CancelScheduledAction(ctx, "a1")
			wantKind(t, err, KindInternal)
			_, err = e.ListScheduledActions(ctx, ListScheduledActionsInput{})
			wantKind(t, err, KindInternal)
			_, err = e.RunDueActions(ctx, time.Now())
			wantKind(t, err, KindInternal)
			// Deleting still works and touches no scheduled actions.
			if deleted, err := e.Delete(ctx, ContentItemMatch{Key: "k1"}); err != nil || !deleted {
				t.Fatalf("delete = %v, %v", deleted, err)
			}
		})
	}
}

func TestCancelScheduledAction(t *testing.T) {
	ctx := context.Background()
	store := newFakeScheduleStore()
	e := newScheduleEngine(store, nil)
	seedPublishable(t, e, "k1", "o1")
	base := scheduleBase()
	first := mustSchedule(t, e, "k1", ActionPublish, base)
	second := mustSchedule(t, e, "k1", ActionUnpublish, base.Add(time.Minute))

	if cancelled, err := e.CancelScheduledAction(ctx, second.ID); err != nil || !cancelled {
		t.Fatalf("cancel pending = %v, %v", cancelled, err)
	}
	if _, err := store.GetScheduledAction(ctx, second.ID); err != ErrNotFound {
		t.Fatalf("cancelled action still stored: %v", err)
	}
	if cancelled, err := e.CancelScheduledAction(ctx, second.ID); err != nil || cancelled {
		t.Fatalf("cancel missing = %v, %v", cancelled, err)
	}
	// The minute is free again.
	mustSchedule(t, e, "k1", ActionArchive, base.Add(time.Minute))

	if _, err := e.RunDueActions(ctx, base); err != nil {
		t.Fatalf("run: %v", err)
	}
	_, err := e.CancelScheduledAction(ctx, first.ID)
	wantKind(t, err, KindNotAcceptable)
	_, err = e.CancelScheduledAction(ctx, "")
	wantKind(t, err, KindBadRequest)
}

func TestListScheduledActions(t *testing.T) {
	ctx := context.Background()
	e := newScheduleEngine(newFakeScheduleStore(), nil)
	seedPublishable(t, e, "k1", "o1")
	seedPublishable(t, e, "k2", "o2")
	base := scheduleBase()
	a1 := mustSchedule(t, e, "k1", ActionPublish, base.Add(time.Minute))
	a2 := mustSchedule(t, e, "k1", ActionUnpublish, base.Add(2*time.Minute))
	a3 := mustSchedule(t, e, "k1", ActionPublish, base.Add(3*time.Minute))
	b1 := mustSchedule(t, e, "k2", ActionArchive, base.Add(2*time.Minute))

	// Pages in (At, ID) order.
	want := []string{a1.ID, a2.ID, b1.ID, a3.ID}
	if a2.ID > b1.ID {
		want = []string{a1.ID, b1.ID, a2.ID, a3.ID}
	}
	page := mustList(t, e, ListScheduledActionsInput{Limit: 3})
	if page.NextCursor == "" || len(page.Items) != 3 {
		t.Fatalf("first page = %v, cursor %q", actionIDs(page.Items), page.NextCursor)
	}
	rest := mustList(t, e, ListScheduledActionsInput{Limit: 3, Cursor: page.NextCursor})
	if rest.NextCursor != "" {
		t.Fatalf("last page has a cursor %q", rest.NextCursor)
	}
	if got := append(actionIDs(page.Items), actionIDs(rest.Items)...); !slices.Equal(got, want) {
		t.Fatalf("pages = %v, want %v", got, want)
	}

	if got := actionIDs(mustList(t, e, ListScheduledActionsInput{Match: ContentItemMatch{Key: "k1"}}).Items); !slices.Equal(got, []string{a1.ID, a2.ID, a3.ID}) {
		t.Fatalf("item list = %v", got)
	}
	from, to := base.Add(2*time.Minute), base.Add(3*time.Minute)
	if got := mustList(t, e, ListScheduledActionsInput{From: &from, To: &to}).Items; len(got) != 2 {
		t.Fatalf("range list = %v", actionIDs(got))
	}
	if _, err := e.RunDueActions(ctx, base.Add(time.Minute)); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := actionIDs(mustList(t, e, ListScheduledActionsInput{Statuses: []ScheduledActionStatus{ActionSucceeded}}).Items); !slices.Equal(got, []string{a1.ID}) {
		t.Fatalf("succeeded list = %v", got)
	}

	for name, input := range map[string]ListScheduledActionsInput{
		"bad cursor": {Cursor: "!"},
		"bad limit":  {Limit: maxChildrenLimit + 1},
		"bad status": {Statuses: []ScheduledActionStatus{"done"}},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := e.ListScheduledActions(ctx, input)
			wantKind(t, err, KindBadRequest)
		})
	}
	_, err := e.ListScheduledActions(ctx, ListScheduledActionsInput{Match: ContentItemMatch{Key: "missing"}})
	wantKind(t, err, KindNotFound)
}

func TestDeleteDropsScheduledActions(t *testing.T) {
	ctx := context.Background()
	store := newFakeScheduleStore()
	e := newScheduleEngine(store, nil)
	seedPublishable(t, e, "k1", "o1")
	seedPublishable(t, e, "k2", "o2")
	folder := place(t, e, folderType, "", "f")
	base := scheduleBase()
	mustSchedule(t, e, "k1", ActionPublish, base)
	mustSchedule(t, e, "k1", ActionUnpublish, base.Add(time.Minute))
	kept := mustSchedule(t, e, "k2", ActionPublish, base)
	if _, err := e.ScheduleAction(ctx, ScheduleActionInput{Match: ContentItemMatch{ID: folder.ID}, Action: ActionPublish, At: base}); err != nil {
		t.Fatalf("schedule folder: %v", err)
	}

	for _, match := range []ContentItemMatch{{Key: "k1"}, {ID: folder.ID}} {
		if deleted, err := e.Delete(ctx, match); err != nil || !deleted {
			t.Fatalf("delete %v = %v, %v", match, deleted, err)
		}
	}
	if got := actionIDs(mustList(t, e, ListScheduledActionsInput{}).Items); !slices.Equal(got, []string{kept.ID}) {
		t.Fatalf("actions after delete = %v, want only %s", got, kept.ID)
	}
}

func TestReplaceContentClearsPendingActions(t *testing.T) {
	ctx := context.Background()
	store := newFakeScheduleStore()
	e := New(Deps{Store: store, Schemas: treeRegistry(), Locales: testLocales, NewID: testNewID(), Role: DeploymentRoleSandbox})
	seedPublishable(t, e, "k1", "o1")
	base := scheduleBase()
	done := mustSchedule(t, e, "k1", ActionPublish, base)
	mustSchedule(t, e, "k1", ActionUnpublish, base.Add(time.Minute))
	if _, err := e.RunDueActions(ctx, base); err != nil {
		t.Fatalf("run: %v", err)
	}

	items, _ := store.ExportAll(ctx)
	export, err := newContentExport(items)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	// An invalid payload is rejected before anything is cleared.
	invalid := *export
	invalid.ItemCount++
	_, err = e.ReplaceContent(ctx, invalid)
	wantKind(t, err, KindBadRequest)
	if got := mustList(t, e, ListScheduledActionsInput{}).Items; len(got) != 2 {
		t.Fatalf("a rejected replacement cleared actions: %v", actionIDs(got))
	}

	if _, err := e.ReplaceContent(ctx, *export); err != nil {
		t.Fatalf("replace: %v", err)
	}
	if got := actionIDs(mustList(t, e, ListScheduledActionsInput{}).Items); !slices.Equal(got, []string{done.ID}) {
		t.Fatalf("actions after replace = %v, want only the executed %s", got, done.ID)
	}
}
