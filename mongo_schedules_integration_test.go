package contentstore

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// newMongoScheduleEngine opens an engine over a fresh database. Scheduled actions need no
// replica set, so unlike the tree tests these run on a standalone server too.
func newMongoScheduleEngine(t *testing.T, url string, role DeploymentRole, opts ...StoreOption) (*Engine, *Persistor) {
	t.Helper()
	store, err := NewMongoStore(context.Background(), url, opts...)
	if err != nil {
		t.Fatalf("NewMongoStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	var n atomic.Int64
	newID := func() string { return fmt.Sprintf("id-%d", n.Add(1)) }
	return New(Deps{Store: store, Schemas: treeRegistry(), Locales: testLocales, NewID: newID, Role: role}), store
}

func collectionNames(t *testing.T, store *Persistor) []string {
	t.Helper()
	names, err := store.coll.Database().ListCollectionNames(context.Background(), bson.D{})
	if err != nil {
		t.Fatalf("list collections: %v", err)
	}
	return names
}

func TestSchedulesAreOptInInMongo(t *testing.T) {
	ctx := context.Background()
	e, store := newMongoScheduleEngine(t, mongoURLForTest(t), "")
	seedPublishable(t, e, "k1", "o1")

	_, err := e.ScheduleAction(ctx, ScheduleActionInput{Match: ContentItemMatch{Key: "k1"}, Action: ActionPublish, At: scheduleBase()})
	wantKind(t, err, KindInternal)
	if err := store.InsertScheduledAction(ctx, &ScheduledAction{ID: "a1"}); !errors.Is(err, errSchedulesDisabled) {
		t.Fatalf("insert without schedules = %v", err)
	}
	if ok, err := store.AcquireScheduleRunner(ctx, "h", time.Now().Add(time.Minute)); ok || !errors.Is(err, errSchedulesDisabled) {
		t.Fatalf("acquire without schedules = %v, %v", ok, err)
	}
	if deleted, err := e.Delete(ctx, ContentItemMatch{Key: "k1"}); err != nil || !deleted {
		t.Fatalf("delete = %v, %v", deleted, err)
	}
	names := collectionNames(t, store)
	if slices.Contains(names, collectionScheduledActions) || slices.Contains(names, collectionScheduleRunner) {
		t.Fatalf("a store without schedules created schedule collections: %v", names)
	}
}

func TestScheduleConflictsAreAtomicInMongo(t *testing.T) {
	ctx := context.Background()
	e, store := newMongoScheduleEngine(t, mongoURLForTest(t), "", WithSchedules())
	seedPublishable(t, e, "k1", "o1")
	base := scheduleBase()

	actions := []ScheduledActionType{ActionPublish, ActionUnpublish, ActionArchive}
	fns := make([]func() error, 9)
	for i := range fns {
		fns[i] = func() error {
			_, err := e.ScheduleAction(ctx, ScheduleActionInput{
				Match: ContentItemMatch{Key: "k1"}, Action: actions[i%3], At: base.Add(time.Duration(i) * time.Second),
			})
			return err
		}
	}
	won := 0
	for _, err := range runConcurrently(fns...) {
		if err == nil {
			won++
			continue
		}
		wantKind(t, err, KindConflict)
	}
	if won != 1 {
		t.Fatalf("%d concurrent actions for one minute were stored, want 1", won)
	}
	mustSchedule(t, e, "k1", ActionUnpublish, base.Add(time.Minute))

	indexes, err := store.coll.Database().Collection(collectionScheduledActions).Indexes().ListSpecifications(ctx)
	if err != nil {
		t.Fatalf("list indexes: %v", err)
	}
	var names []string
	for _, index := range indexes {
		names = append(names, index.Name)
	}
	for _, want := range []string{"item_at_pending_unique", "status_at", "itemId_at"} {
		if !slices.Contains(names, want) {
			t.Fatalf("index %s missing: %v", want, names)
		}
	}
}

func TestRunDueActionsInMongo(t *testing.T) {
	ctx := context.Background()
	url := mongoURLForTest(t)
	e, _ := newMongoScheduleEngine(t, url, "", WithSchedules())
	seedPublishable(t, e, "k1", "o1")
	seedPublishable(t, e, "k2", "o2")
	base := scheduleBase()
	publish := mustSchedule(t, e, "k1", ActionPublish, base)
	unpublish := mustSchedule(t, e, "k1", ActionUnpublish, base.Add(time.Minute))
	later := mustSchedule(t, e, "k1", ActionArchive, base.Add(time.Hour))
	other := mustSchedule(t, e, "k2", ActionPublish, base.Add(time.Hour))

	run := mustRun(t, e, base.Add(time.Minute))
	if got := actionIDs(run.Actions); !slices.Equal(got, []string{publish.ID, unpublish.ID}) {
		t.Fatalf("run = %v", got)
	}
	if got := getItem(t, e, publish.ItemID).State(); got != StateDraft {
		t.Fatalf("state = %s", got)
	}
	history := mustList(t, e, ListScheduledActionsInput{Match: ContentItemMatch{Key: "k1"}, Statuses: []ScheduledActionStatus{ActionSucceeded}})
	if got := actionIDs(history.Items); !slices.Equal(got, []string{publish.ID, unpublish.ID}) {
		t.Fatalf("history = %v", got)
	}
	for _, a := range history.Items {
		if a.ExecutedAt == nil || !a.At.Equal(map[string]time.Time{publish.ID: base, unpublish.ID: base.Add(time.Minute)}[a.ID]) {
			t.Fatalf("stored action = %#v", a)
		}
	}
	_, err := e.CancelScheduledAction(ctx, publish.ID)
	wantKind(t, err, KindNotAcceptable)

	// Deleting an item deletes its actions.
	if _, err := e.Delete(ctx, ContentItemMatch{Key: "k1"}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if got := actionIDs(mustList(t, e, ListScheduledActionsInput{}).Items); !slices.Equal(got, []string{other.ID}) {
		t.Fatalf("actions after delete = %v (deleted item's %s must be gone)", got, later.ID)
	}

	// A sandbox replacement clears pending actions and keeps the history.
	sandbox, store := newMongoScheduleEngine(t, url, DeploymentRoleSandbox, WithSchedules())
	mustRun(t, sandbox, base.Add(time.Hour))
	seedPublishable(t, sandbox, "k3", "o3")
	mustSchedule(t, sandbox, "k3", ActionPublish, base.Add(2*time.Hour))
	items, err := store.ExportAll(ctx)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	export, err := newContentExport(items)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if _, err := sandbox.ReplaceContent(ctx, *export); err != nil {
		t.Fatalf("replace: %v", err)
	}
	if got := actionIDs(mustList(t, sandbox, ListScheduledActionsInput{}).Items); !slices.Equal(got, []string{other.ID}) {
		t.Fatalf("actions after replace = %v, want only the executed %s", got, other.ID)
	}
}

func TestConcurrentRunnersInMongo(t *testing.T) {
	url := mongoURLForTest(t)
	e, _ := newMongoScheduleEngine(t, url, "", WithSchedules())
	base := scheduleBase()
	var want []string
	for i := range 20 {
		key := fmt.Sprintf("k%d", i)
		seedPublishable(t, e, key, key)
		want = append(want, mustSchedule(t, e, key, ActionPublish, base.Add(time.Duration(i%3)*time.Minute)).ID)
	}

	// Each runner has its own store, as replicas of an application would.
	var mu sync.Mutex
	var ran []string
	fns := make([]func() error, 4)
	for i := range fns {
		runner, _ := newMongoScheduleEngine(t, url, "", WithSchedules())
		fns[i] = func() error {
			run, err := runner.RunDueActions(context.Background(), base.Add(time.Hour))
			if err != nil {
				return err
			}
			mu.Lock()
			defer mu.Unlock()
			for _, a := range run.Actions {
				if a.Status != ActionSucceeded {
					return fmt.Errorf("action %s: %s %s", a.ID, a.Status, a.Error)
				}
				ran = append(ran, a.ID)
			}
			return nil
		}
	}
	for _, err := range runConcurrently(fns...) {
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	}
	slices.Sort(ran)
	slices.Sort(want)
	if !slices.Equal(ran, want) {
		t.Fatalf("ran %d actions %v, want each of %d once", len(ran), ran, len(want))
	}
}

func TestScheduleRunnerLeaseInMongo(t *testing.T) {
	ctx := context.Background()
	_, store := newMongoScheduleEngine(t, mongoURLForTest(t), "", WithSchedules())
	acquire := func(holder string, until time.Time) bool {
		t.Helper()
		ok, err := store.AcquireScheduleRunner(ctx, holder, until)
		if err != nil {
			t.Fatalf("acquire %s: %v", holder, err)
		}
		return ok
	}
	soon := time.Now().Add(time.Minute)
	if !acquire("a", soon) || acquire("b", soon) || !acquire("a", soon) {
		t.Fatal("only the holder may take or renew a valid lease")
	}
	if err := store.ReleaseScheduleRunner(ctx, "b"); err != nil {
		t.Fatalf("release b: %v", err)
	}
	if acquire("b", soon) {
		t.Fatal("another holder's release must not free the lease")
	}
	if err := store.ReleaseScheduleRunner(ctx, "a"); err != nil {
		t.Fatalf("release a: %v", err)
	}
	if !acquire("b", time.Now().Add(-time.Second)) {
		t.Fatal("a released lease must be free")
	}
	if !acquire("a", soon) {
		t.Fatal("an expired lease must be free")
	}
}

func TestCancelAndClaimExcludeEachOtherInMongo(t *testing.T) {
	ctx := context.Background()
	e, store := newMongoScheduleEngine(t, mongoURLForTest(t), "", WithSchedules())
	seedPublishable(t, e, "k1", "o1")
	base := scheduleBase()

	claimed := *mustSchedule(t, e, "k1", ActionPublish, base)
	claimed.Status = ActionRunning
	if err := store.UpdateScheduledAction(ctx, &claimed, ActionPending); err != nil {
		t.Fatalf("claim: %v", err)
	}
	_, err := e.CancelScheduledAction(ctx, claimed.ID)
	wantKind(t, err, KindNotAcceptable)

	cancelled := mustSchedule(t, e, "k1", ActionUnpublish, base.Add(time.Minute))
	if ok, err := e.CancelScheduledAction(ctx, cancelled.ID); err != nil || !ok {
		t.Fatalf("cancel = %v, %v", ok, err)
	}
	if done, err := e.runAction(ctx, cancelled); err != nil || done != nil {
		t.Fatalf("a cancelled action was claimed: %#v, %v", done, err)
	}
}

func TestRunDueActionsPublishesTreeItemsInMongo(t *testing.T) {
	ctx := context.Background()
	e, _ := newMongoTreeEngine(t, WithSchedules())
	folder := place(t, e, folderType, "", "kb")
	doc := place(t, e, documentType, folder.ID, "intro.md")
	base := scheduleBase()
	for _, id := range []string{doc.ID, folder.ID} {
		if _, err := e.ScheduleAction(ctx, ScheduleActionInput{Match: ContentItemMatch{ID: id}, Action: ActionPublish, At: base}); err != nil {
			t.Fatalf("schedule: %v", err)
		}
	}
	mustRun(t, e, base)
	got, err := e.GetTreeItem(ctx, SnapshotViewPublished, TreeItemInput{Path: "/kb/intro.md"})
	if err != nil {
		t.Fatalf("published tree read: %v", err)
	}
	if got.Item.ID != doc.ID {
		t.Fatalf("published item = %#v", got.Item)
	}
}
