package contentstore

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

func mustRun(t *testing.T, e *Engine, now time.Time) *ScheduleRun {
	t.Helper()
	run, err := e.RunDueActions(context.Background(), now)
	if err != nil {
		t.Fatalf("run at %s: %v", now, err)
	}
	return run
}

func ops(changes []ContentChange) []string {
	out := make([]string, len(changes))
	for i, c := range changes {
		out[i] = c.Op
	}
	return out
}

func TestRunDueActionsRunsInOrder(t *testing.T) {
	n := &recordingNotifier{}
	store := newFakeScheduleStore()
	e := newScheduleEngine(store, n)
	seedPublishable(t, e, "k1", "o1")
	base := scheduleBase()
	publish := mustSchedule(t, e, "k1", ActionPublish, base.Add(time.Minute))
	unpublish := mustSchedule(t, e, "k1", ActionUnpublish, base.Add(2*time.Minute))
	republish := mustSchedule(t, e, "k1", ActionPublish, base.Add(3*time.Minute))
	archive := mustSchedule(t, e, "k1", ActionArchive, base.Add(4*time.Minute))

	if run := mustRun(t, e, base); run.Busy || len(run.Actions) != 0 {
		t.Fatalf("nothing is due yet, got %#v", run)
	}

	// Two due actions catch up in order; the later ones are not due yet.
	run := mustRun(t, e, base.Add(2*time.Minute+30*time.Second))
	if got := actionIDs(run.Actions); !slices.Equal(got, []string{publish.ID, unpublish.ID}) {
		t.Fatalf("run = %v", got)
	}
	for _, a := range run.Actions {
		if a.Status != ActionSucceeded || a.ExecutedAt == nil || a.Error != "" {
			t.Fatalf("unexpected outcome %#v", a)
		}
		if stored := store.action(t, a.ID); stored.Status != ActionSucceeded {
			t.Fatalf("stored status = %s", stored.Status)
		}
	}
	if got := getItem(t, e, "id-1").State(); got != StateDraft {
		t.Fatalf("state after publish+unpublish = %s", got)
	}
	if got := ops(n.changes); !slices.Equal(got, []string{OpPublish, OpUnpublish}) {
		t.Fatalf("events = %v", got)
	}

	run = mustRun(t, e, base.Add(time.Hour))
	if got := actionIDs(run.Actions); !slices.Equal(got, []string{republish.ID, archive.ID}) {
		t.Fatalf("second run = %v", got)
	}
	if got := getItem(t, e, "id-1").State(); got != StateArchived {
		t.Fatalf("state after archive = %s", got)
	}
	if run := mustRun(t, e, base.Add(time.Hour)); len(run.Actions) != 0 {
		t.Fatalf("actions ran twice: %v", actionIDs(run.Actions))
	}
}

func TestRunDueActionsPublishesTreeItems(t *testing.T) {
	store := newFakeScheduleStore()
	e := newScheduleEngine(store, nil)
	folder := place(t, e, folderType, "", "kb")
	doc := place(t, e, documentType, folder.ID, "intro.md")
	base := scheduleBase()
	for _, id := range []string{folder.ID, doc.ID} {
		if _, err := e.ScheduleAction(context.Background(), ScheduleActionInput{Match: ContentItemMatch{ID: id}, Action: ActionPublish, At: base}); err != nil {
			t.Fatalf("schedule: %v", err)
		}
	}
	mustRun(t, e, base)
	for _, id := range []string{folder.ID, doc.ID} {
		if got := getItem(t, e, id).State(); got != StatePublished {
			t.Fatalf("%s state = %s", id, got)
		}
	}
}

func TestRunDueActionsRecordsFailures(t *testing.T) {
	ctx := context.Background()
	store := newFakeScheduleStore()
	e := newScheduleEngine(store, nil)
	// A draft without the required filterSelection cannot be published.
	if _, err := e.SaveDraft(ctx, SaveDraftInput{Match: ContentItemMatch{Key: "incomplete"}, Type: testType, Fields: ContentFields{}}); err != nil {
		t.Fatalf("save: %v", err)
	}
	seedPublishable(t, e, "gone", "o1")
	base := scheduleBase()
	invalid := mustSchedule(t, e, "incomplete", ActionPublish, base)
	orphan := mustSchedule(t, e, "gone", ActionPublish, base)
	// Deleted behind the engine's back, so its action is left behind.
	if err := store.fakeStore.Delete(ctx, orphan.ItemID); err != nil {
		t.Fatalf("delete: %v", err)
	}

	run := mustRun(t, e, base)
	if len(run.Actions) != 2 {
		t.Fatalf("run = %#v", run.Actions)
	}
	for id, want := range map[string]string{invalid.ID: `required field "filterSelection"`, orphan.ID: "not found"} {
		got := store.action(t, id)
		if got.Status != ActionFailed || got.ExecutedAt == nil || !strings.Contains(got.Error, want) {
			t.Fatalf("action %s = %#v, want failed with %q", id, got, want)
		}
	}
	if run := mustRun(t, e, base.Add(time.Hour)); len(run.Actions) != 0 {
		t.Fatalf("failed actions were retried: %v", actionIDs(run.Actions))
	}
}

// failingUpsertStore fails every Upsert of one item, as an unavailable store would.
type failingUpsertStore struct {
	*fakeScheduleStore
	failID string
}

func (f *failingUpsertStore) Upsert(ctx context.Context, item *ContentItem, expected *int64) error {
	if item.ID == f.failID {
		return errors.New("store unavailable")
	}
	return f.fakeScheduleStore.Upsert(ctx, item, expected)
}

func TestRunDueActionsRetriesInternalErrorsInOrder(t *testing.T) {
	store := &failingUpsertStore{fakeScheduleStore: newFakeScheduleStore()}
	e := newScheduleEngine(store, nil)
	seedPublishable(t, e, "a", "o1")
	seedPublishable(t, e, "b", "o2")
	base := scheduleBase()
	publishA := mustSchedule(t, e, "a", ActionPublish, base)
	unpublishA := mustSchedule(t, e, "a", ActionUnpublish, base.Add(time.Minute))
	publishB := mustSchedule(t, e, "b", ActionPublish, base)
	store.failID = publishA.ItemID

	run := mustRun(t, e, base.Add(time.Hour))
	byID := map[string]ScheduledAction{}
	for _, a := range run.Actions {
		byID[a.ID] = a
	}
	if len(run.Actions) != 2 || byID[publishB.ID].Status != ActionSucceeded {
		t.Fatalf("run = %#v", run.Actions)
	}
	if got := byID[publishA.ID]; got.Status != ActionPending || !strings.Contains(got.Error, "store unavailable") || got.ExecutedAt != nil {
		t.Fatalf("failed action = %#v, want pending with the error", got)
	}
	if got := store.action(t, unpublishA.ID); got.Status != ActionPending || got.Error != "" {
		t.Fatalf("the item's later action ran or changed: %#v", got)
	}

	store.failID = ""
	run = mustRun(t, e, base.Add(time.Hour))
	if got := actionIDs(run.Actions); !slices.Equal(got, []string{publishA.ID, unpublishA.ID}) {
		t.Fatalf("retry run = %v", got)
	}
	if got := store.action(t, publishA.ID); got.Status != ActionSucceeded || got.Error != "" {
		t.Fatalf("retried action = %#v", got)
	}
}

func TestRunDueActionsWaitsForTheLease(t *testing.T) {
	ctx := context.Background()
	store := newFakeScheduleStore()
	e := newScheduleEngine(store, nil)
	seedPublishable(t, e, "k1", "o1")
	base := scheduleBase()
	action := mustSchedule(t, e, "k1", ActionPublish, base)

	if ok, _ := store.AcquireScheduleRunner(ctx, "other", time.Now().Add(time.Minute)); !ok {
		t.Fatal("acquire")
	}
	if run := mustRun(t, e, base); !run.Busy || len(run.Actions) != 0 {
		t.Fatalf("run while leased = %#v", run)
	}
	if got := store.action(t, action.ID); got.Status != ActionPending {
		t.Fatalf("action ran while another runner held the lease: %#v", got)
	}

	// An expired lease is taken over.
	if ok, _ := store.AcquireScheduleRunner(ctx, "other", time.Now().Add(-time.Second)); !ok {
		t.Fatal("renew")
	}
	if run := mustRun(t, e, base); run.Busy || len(run.Actions) != 1 {
		t.Fatalf("run after expiry = %#v", run)
	}
	if store.holder != "" {
		t.Fatalf("the run kept the lease: %q", store.holder)
	}
}

// leaseLosingStore loses the runner lease after the first acquisition.
type leaseLosingStore struct {
	*fakeScheduleStore
	acquired int
}

func (s *leaseLosingStore) AcquireScheduleRunner(ctx context.Context, holder string, until time.Time) (bool, error) {
	s.acquired++
	return s.acquired == 1, nil
}

func TestRunDueActionsStopsWhenTheLeaseIsLost(t *testing.T) {
	store := &leaseLosingStore{fakeScheduleStore: newFakeScheduleStore()}
	e := newScheduleEngine(store, nil)
	seedPublishable(t, e, "k1", "o1")
	base := scheduleBase()
	action := mustSchedule(t, e, "k1", ActionPublish, base)

	run, err := e.RunDueActions(context.Background(), base)
	wantKind(t, err, KindConflict)
	if run == nil || len(run.Actions) != 0 {
		t.Fatalf("run = %#v", run)
	}
	if got := store.action(t, action.ID); got.Status != ActionPending {
		t.Fatalf("action = %#v", got)
	}
}

func TestRunDueActionsRepeatsAbandonedClaims(t *testing.T) {
	ctx := context.Background()
	store := newFakeScheduleStore()
	e := newScheduleEngine(store, nil)
	seedPublishable(t, e, "k1", "o1")
	base := scheduleBase()
	action := mustSchedule(t, e, "k1", ActionPublish, base)
	// A run claimed it and stopped before recording the outcome.
	claimed := *action
	claimed.Status = ActionRunning
	if err := store.UpdateScheduledAction(ctx, &claimed, ActionPending); err != nil {
		t.Fatalf("claim: %v", err)
	}

	if run := mustRun(t, e, base); len(run.Actions) != 1 || run.Actions[0].Status != ActionSucceeded {
		t.Fatalf("run = %#v", run)
	}
	if got := getItem(t, e, action.ItemID).State(); got != StatePublished {
		t.Fatalf("state = %s", got)
	}
}

func TestRunActionSkipsCancelledActions(t *testing.T) {
	ctx := context.Background()
	store := newFakeScheduleStore()
	e := newScheduleEngine(store, nil)
	seedPublishable(t, e, "k1", "o1")
	action := mustSchedule(t, e, "k1", ActionPublish, scheduleBase())
	if cancelled, err := e.CancelScheduledAction(ctx, action.ID); err != nil || !cancelled {
		t.Fatalf("cancel = %v, %v", cancelled, err)
	}
	// The run read the action before it was cancelled.
	done, err := e.runAction(ctx, action)
	if err != nil || done != nil {
		t.Fatalf("runAction = %#v, %v", done, err)
	}
	if got := getItem(t, e, action.ItemID).State(); got != StateDraft {
		t.Fatalf("a cancelled action ran: state %s", got)
	}
}
