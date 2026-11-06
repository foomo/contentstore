package contentstore

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// newMongoTreeEngine opens a tree engine over a fresh database on a local replica set.
// Tree writes use transactions, so the tests skip on a standalone server.
func newMongoTreeEngine(t *testing.T) (*Engine, *Persistor) {
	t.Helper()
	ctx := context.Background()
	store, err := NewMongoStore(ctx, mongoURLForTest(t))
	if err != nil {
		t.Fatalf("NewMongoStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	var hello struct {
		SetName string `bson:"setName"`
	}
	if err := store.client.Database("admin").RunCommand(ctx, bson.D{{Key: "hello", Value: 1}}).Decode(&hello); err != nil {
		t.Fatalf("hello: %v", err)
	}
	if hello.SetName == "" {
		t.Skip("MongoDB is not a replica set: tree transactions are unavailable")
	}
	var n atomic.Int64
	newID := func() string { return fmt.Sprintf("id-%d", n.Add(1)) }
	return New(Deps{Store: store, Schemas: tagRegistry(), Locales: testLocales, NewID: newID}), store
}

func TestTreeWritesInMongo(t *testing.T) {
	ctx := context.Background()
	e, store := newMongoTreeEngine(t)
	kb := place(t, e, folderType, "", "kb")
	doc := place(t, e, documentType, kb.ID, "doc")
	place(t, e, documentType, kb.ID, "b-doc")

	_, err := e.SaveDraft(ctx, SaveDraftInput{Type: documentType, ParentID: kb.ID, Name: "doc", Fields: titleFields("x")})
	wantKind(t, err, KindConflict)
	_, err = e.SaveDraft(ctx, SaveDraftInput{Type: folderType, Name: "kb", Fields: ContentFields{}})
	wantKind(t, err, KindConflict)

	children, err := store.FindChildren(ctx, ChildQuery{ParentID: kb.ID, Fields: []FieldID{}})
	if err != nil || len(children) != 2 || children[0].Name != "b-doc" || children[1].Name != "doc" {
		t.Fatalf("children = %v, err = %v", children, err)
	}
	if children[1].Draft == nil || children[1].Draft.Revision != 1 || children[1].Draft.Fields != nil {
		t.Fatalf("structure-only projection must keep revisions and drop fields: %+v", children[1].Draft)
	}
	roots, err := store.FindChildren(ctx, ChildQuery{Lifecycle: ChildrenLive})
	if err != nil || len(roots) != 1 || roots[0].ID != kb.ID {
		t.Fatalf("root children = %v, err = %v", roots, err)
	}

	if _, err := e.Move(ctx, MoveInput{Match: ContentItemMatch{ID: doc.ID}, ParentID: new(""), Name: "top"}); err != nil {
		t.Fatalf("move to root: %v", err)
	}
	var raw bson.M
	if err := store.coll.FindOne(ctx, bson.D{{Key: "_id", Value: doc.ID}}).Decode(&raw); err != nil {
		t.Fatalf("raw: %v", err)
	}
	if _, ok := raw["parentId"]; ok || raw["name"] != "top" {
		t.Fatalf("a root item must store no parentId: %v", raw)
	}

	_, err = e.Delete(ctx, ContentItemMatch{ID: kb.ID})
	wantKind(t, err, KindNotAcceptable)
}

// TestTreeNameIndexInMongo checks the unique index backstops the engine's name check.
func TestTreeNameIndexInMongo(t *testing.T) {
	ctx := context.Background()
	_, store := newMongoTreeEngine(t)
	for i, id := range []string{"a", "b"} {
		err := store.Upsert(ctx, &ContentItem{ID: id, Type: documentType, ParentID: "p", Name: "same", Draft: &ContentSnapshot{Revision: 1}}, nil)
		if i == 0 && err != nil {
			t.Fatalf("first upsert: %v", err)
		}
		if i == 1 && err == nil {
			t.Fatal("duplicate (parentId, name) must be rejected by the index")
		}
	}
	// Items without a name never collide (every item of a non-tree type).
	for _, id := range []string{"c", "d"} {
		if err := store.Upsert(ctx, &ContentItem{ID: id, Type: testType, Draft: &ContentSnapshot{Revision: 1}}, nil); err != nil {
			t.Fatalf("unnamed upsert: %v", err)
		}
	}
}

// TestSetFieldKeyUpsertKeepsTypeInMongo simulates the outcome of a create race: a key
// upsert addressed to one type must not rewrite an item of another type holding the key.
func TestSetFieldKeyUpsertKeepsTypeInMongo(t *testing.T) {
	ctx := context.Background()
	e, store := newMongoTreeEngine(t)
	doc, err := e.SaveDraft(ctx, SaveDraftInput{Match: ContentItemMatch{Key: "shared"}, Type: documentType, Name: "doc", Fields: titleFields("x")})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	_, err = store.SetField(ctx, FieldUpdate{Match: ContentItemMatch{Key: "shared"}, Type: testType, FieldID: "metaTitle", Locale: "de", Value: "x", NewID: "other"})
	if err == nil {
		t.Fatal("key upsert for another type must fail")
	}
	if got := getItem(t, e, doc.ID); got.Type != documentType || got.Draft.Fields["metaTitle"] != nil {
		t.Fatalf("item was rewritten: %+v", got)
	}
}

// runConcurrently starts every fn at once and returns their errors.
func runConcurrently(fns ...func() error) []error {
	errs := make([]error, len(fns))
	var start, done sync.WaitGroup
	start.Add(1)
	for i, fn := range fns {
		done.Add(1)
		go func() {
			defer done.Done()
			start.Wait()
			errs[i] = fn()
		}()
	}
	start.Done()
	done.Wait()
	return errs
}

func TestTreeConcurrentOppositeMovesInMongo(t *testing.T) {
	ctx := context.Background()
	e, _ := newMongoTreeEngine(t)
	for round := range 10 {
		a := place(t, e, folderType, "", fmt.Sprintf("a-%d", round))
		b := place(t, e, folderType, "", fmt.Sprintf("b-%d", round))
		errs := runConcurrently(
			func() error {
				_, err := e.Move(ctx, MoveInput{Match: ContentItemMatch{ID: a.ID}, ParentID: new(b.ID)})
				return err
			},
			func() error {
				_, err := e.Move(ctx, MoveInput{Match: ContentItemMatch{ID: b.ID}, ParentID: new(a.ID)})
				return err
			},
		)
		failed := 0
		for _, err := range errs {
			if err != nil {
				wantKind(t, err, KindBadRequest)
				failed++
			}
		}
		if failed != 1 {
			t.Fatalf("round %d: exactly one opposite move must win, errors: %v", round, errs)
		}
		if _, err := e.ancestors(ctx, getItem(t, e, a.ID)); err != nil {
			t.Fatalf("round %d: tree is corrupt: %v", round, err)
		}
	}
}

func TestTreeCreateRacesArchiveInMongo(t *testing.T) {
	ctx := context.Background()
	e, _ := newMongoTreeEngine(t)
	for round := range 10 {
		folder := place(t, e, folderType, "", fmt.Sprintf("f-%d", round))
		runConcurrently(
			func() error {
				_, err := e.SaveDraft(ctx, SaveDraftInput{Type: documentType, ParentID: folder.ID, Name: "doc", Fields: titleFields("x")})
				return err
			},
			func() error { _, err := e.Archive(ctx, ContentItemMatch{ID: folder.ID}); return err },
		)
		archived := getItem(t, e, folder.ID).ArchivedAt != nil
		children, err := e.tree.FindChildren(ctx, ChildQuery{ParentID: folder.ID, Lifecycle: ChildrenLive, Fields: []FieldID{}})
		if err != nil {
			t.Fatalf("children: %v", err)
		}
		if archived && len(children) > 0 {
			t.Fatalf("round %d: live child under an archived folder", round)
		}
	}
}

func TestTreePublishRacesMoveInMongo(t *testing.T) {
	ctx := context.Background()
	e, _ := newMongoTreeEngine(t)
	from := place(t, e, folderType, "", "from")
	to := place(t, e, folderType, "", "to")
	for round := range 10 {
		doc := place(t, e, documentType, from.ID, fmt.Sprintf("doc-%d", round))
		errs := runConcurrently(
			func() error { _, err := e.Publish(ctx, ContentItemMatch{ID: doc.ID}); return err },
			func() error {
				_, err := e.Move(ctx, MoveInput{Match: ContentItemMatch{ID: doc.ID}, ParentID: new(to.ID)})
				return err
			},
		)
		if err := errors.Join(errs...); err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
		if got := getItem(t, e, doc.ID); got.ParentID != to.ID || got.Published == nil {
			t.Fatalf("round %d: publish and move must both survive: parent=%q published=%v", round, got.ParentID, got.Published != nil)
		}
	}
}

// TestTreeReadsInMongo runs the tree reads against the persistor's filters, projections
// and indexes (see buildReadTree for the tree).
func TestTreeReadsInMongo(t *testing.T) {
	ctx := context.Background()
	e, _ := newMongoTreeEngine(t)
	items := buildReadTree(t, e)
	kb := items["kb"].ID

	var got []string
	input := ListChildrenInput{ParentID: kb, Limit: 1, Fields: []FieldID{"title"}}
	for {
		page, err := e.ListChildren(ctx, SnapshotViewPreview, input)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		for _, item := range page.Items {
			if _, ok := item.Fields["body"]; ok || item.Fields["title"]["de"] != item.Name {
				t.Fatalf("projected fields of %s = %v", item.Name, item.Fields)
			}
		}
		got = append(got, names(page.Items)...)
		if page.NextCursor == "" {
			break
		}
		input.Cursor = page.NextCursor
	}
	if want := []string{"a", "b", "drafts"}; !slices.Equal(got, want) {
		t.Fatalf("paged preview children = %v, want %v", got, want)
	}
	page, err := e.ListChildren(ctx, SnapshotViewPublished, ListChildrenInput{ParentID: kb})
	if err != nil || !slices.Equal(names(page.Items), []string{"a"}) || page.Items[0].Fields["body"]["de"] != "# A" {
		t.Fatalf("published children = %+v, err = %v", page, err)
	}

	item, err := e.GetTreeItem(ctx, SnapshotViewPreview, TreeItemInput{Path: "/kb/drafts/d"})
	if err != nil || item.Item.ID != items["d"].ID || item.Path != "/kb/drafts/d" || len(item.Ancestors) != 2 {
		t.Fatalf("path lookup = %+v, err = %v", item, err)
	}
	_, err = e.GetTreeItem(ctx, SnapshotViewPublished, TreeItemInput{Path: "/kb/drafts/d"})
	wantKind(t, err, KindNotFound)

	snapshot, err := e.GetSnapshot(ctx, SnapshotViewPublished, SnapshotInput{Types: []ContentType{documentType}})
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	var published []string
	for _, item := range snapshot.Items {
		published = append(published, item.Name)
	}
	slices.Sort(published)
	if want := []string{"a", "readme"}; !slices.Equal(published, want) {
		t.Fatalf("published snapshot = %v, want %v", published, want)
	}
}
