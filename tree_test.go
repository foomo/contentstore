package contentstore

import (
	"context"
	"strings"
	"testing"
)

const (
	folderType   ContentType = "folder"
	documentType ContentType = "document"
)

// treeRegistry adds a knowledge-base-like tree to the test registry: folders contain
// folders and documents; the legacy test type stays outside the tree.
func treeRegistry() *Registry {
	r := testRegistry()
	folder := NewSchema(folderType, "1", []FieldDefinition{{ID: "title", Type: FieldTypeSymbol}})
	folder.ChildTypes = []ContentType{folderType, documentType}
	r.Register(folder)
	r.Register(NewSchema(documentType, "1", []FieldDefinition{
		{ID: "title", Type: FieldTypeSymbol, Required: true},
		{ID: "body", Type: FieldTypeText, EditingFormat: FieldEditingFormatMarkdown, MaxLength: new(1_000_000)},
		{ID: "frontMatter", Type: FieldTypeJSON},
	}))
	return r
}

func newTreeEngine(store Store) *Engine {
	return New(Deps{Store: store, Schemas: treeRegistry(), Locales: testLocales, NewID: testNewID()})
}

func titleFields(title string) ContentFields {
	return mkFields(map[string]map[string]any{"title": {"de": title}})
}

// place creates a tree item of type t under parentID ("" = root) and fails the test on error.
func place(t *testing.T, e *Engine, contentType ContentType, parentID, name string) *ContentItem {
	t.Helper()
	item, err := e.SaveDraft(context.Background(), SaveDraftInput{Type: contentType, ParentID: parentID, Name: name, Fields: titleFields(name)})
	if err != nil {
		t.Fatalf("place %s %q under %q: %v", contentType, name, parentID, err)
	}
	return item
}

func getItem(t *testing.T, e *Engine, id string) *ContentItem {
	t.Helper()
	item, err := e.GetItem(context.Background(), ContentItemMatch{ID: id})
	if err != nil {
		t.Fatalf("get %q: %v", id, err)
	}
	return item
}

func TestTreeCreatePlacesItems(t *testing.T) {
	store := newFakeStore()
	e := newTreeEngine(store)
	kb := place(t, e, folderType, "", "kb")
	tables := place(t, e, folderType, kb.ID, "tables")
	users := place(t, e, documentType, tables.ID, "users")
	rootDoc := place(t, e, documentType, "", "readme")

	if got := getItem(t, e, users.ID); got.ParentID != tables.ID || got.Name != "users" || got.Draft.Revision != 1 {
		t.Fatalf("document placement = parent %q name %q revision %d", got.ParentID, got.Name, got.Draft.Revision)
	}
	if got := getItem(t, e, rootDoc.ID); got.ParentID != "" || got.Name != "readme" {
		t.Fatalf("root document placement = parent %q name %q", got.ParentID, got.Name)
	}
	// Same name under different parents is fine.
	place(t, e, documentType, kb.ID, "users")
	if store.treeTransactions != 5 {
		t.Fatalf("expected every tree create in a tree transaction, got %d", store.treeTransactions)
	}
}

func TestTreeCreateRejects(t *testing.T) {
	ctx := context.Background()
	e := newTreeEngine(newFakeStore())
	kb := place(t, e, folderType, "", "kb")
	doc := place(t, e, documentType, kb.ID, "doc")
	archived := place(t, e, folderType, kb.ID, "old")
	if _, err := e.Archive(ctx, ContentItemMatch{ID: archived.ID}); err != nil {
		t.Fatalf("archive: %v", err)
	}

	tests := []struct {
		name  string
		input SaveDraftInput
		kind  Kind
	}{
		{"missing name", SaveDraftInput{Type: folderType, ParentID: kb.ID}, KindBadRequest},
		{"empty root name", SaveDraftInput{Type: documentType}, KindBadRequest},
		{"dot", SaveDraftInput{Type: documentType, Name: "."}, KindBadRequest},
		{"dot dot", SaveDraftInput{Type: documentType, Name: ".."}, KindBadRequest},
		{"slash", SaveDraftInput{Type: documentType, Name: "a/b"}, KindBadRequest},
		{"control character", SaveDraftInput{Type: documentType, Name: "a\x00b"}, KindBadRequest},
		{"invalid utf-8", SaveDraftInput{Type: documentType, Name: "a\xffb"}, KindBadRequest},
		{"too long", SaveDraftInput{Type: documentType, Name: strings.Repeat("a", 257)}, KindBadRequest},
		{"placement on a non-tree type", SaveDraftInput{Type: testType, Name: "x"}, KindBadRequest},
		{"missing parent", SaveDraftInput{Type: documentType, ParentID: "nope", Name: "x"}, KindBadRequest},
		{"archived parent", SaveDraftInput{Type: documentType, ParentID: archived.ID, Name: "x"}, KindNotAcceptable},
		{"parent does not accept the type", SaveDraftInput{Type: documentType, ParentID: doc.ID, Name: "x"}, KindBadRequest},
		{"name taken", SaveDraftInput{Type: documentType, ParentID: kb.ID, Name: "doc"}, KindConflict},
		{"name taken by an archived sibling", SaveDraftInput{Type: documentType, ParentID: kb.ID, Name: "old"}, KindConflict},
		{"name taken at root", SaveDraftInput{Type: folderType, Name: "kb"}, KindConflict},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.input.Fields = titleFields("x")
			_, err := e.SaveDraft(ctx, tt.input)
			wantKind(t, err, tt.kind)
		})
	}
	if name := strings.Repeat("a", 256); place(t, e, documentType, kb.ID, name).Name != name {
		t.Fatal("a 256-byte name must be accepted")
	}
}

func TestTreeSaveDraftUpdateKeepsPlacement(t *testing.T) {
	ctx := context.Background()
	e := newTreeEngine(newFakeStore())
	kb := place(t, e, folderType, "", "kb")
	other := place(t, e, folderType, "", "other")
	doc := place(t, e, documentType, kb.ID, "doc")

	for _, input := range []SaveDraftInput{
		{Match: ContentItemMatch{ID: doc.ID}, Fields: titleFields("v2")},
		{Match: ContentItemMatch{ID: doc.ID}, ParentID: kb.ID, Name: "doc", Fields: titleFields("v3")},
	} {
		if _, err := e.SaveDraft(ctx, input); err != nil {
			t.Fatalf("update keeping placement: %v", err)
		}
	}
	for _, input := range []SaveDraftInput{
		{Match: ContentItemMatch{ID: doc.ID}, Name: "renamed", Fields: titleFields("x")},
		{Match: ContentItemMatch{ID: doc.ID}, ParentID: other.ID, Fields: titleFields("x")},
	} {
		_, err := e.SaveDraft(ctx, input)
		wantKind(t, err, KindBadRequest)
	}
	if got := getItem(t, e, doc.ID); got.ParentID != kb.ID || got.Name != "doc" || got.Draft.Revision != 3 {
		t.Fatalf("after updates: parent %q name %q revision %d", got.ParentID, got.Name, got.Draft.Revision)
	}
}

func TestTreeSetFieldValue(t *testing.T) {
	ctx := context.Background()
	rec := &recordingStore{inner: newFakeStore()}
	e := newTreeEngine(rec)

	_, err := e.SetFieldValue(ctx, SetFieldValueInput{Match: ContentItemMatch{Key: "new"}, Type: documentType, FieldID: "title", Locale: "de", Value: "x"})
	wantKind(t, err, KindBadRequest)

	doc, err := e.SaveDraft(ctx, SaveDraftInput{Match: ContentItemMatch{Key: "doc-key"}, Type: documentType, Name: "doc", Fields: titleFields("v1")})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	rec.calls = nil
	if _, err := e.SetFieldValue(ctx, SetFieldValueInput{Match: ContentItemMatch{Key: "doc-key"}, FieldID: "body", Locale: "de", Value: "# Body"}); err != nil {
		t.Fatalf("set field: %v", err)
	}
	want := "SetField(id=" + doc.ID + ",key=,type=document,field=body,locale=de,"
	if len(rec.calls) != 2 || !strings.HasPrefix(rec.calls[1], want) {
		t.Fatalf("tree items must be written by ID, calls: %v", rec.calls)
	}
	if got := getItem(t, e, doc.ID); got.Draft.Fields["body"]["de"] != "# Body" || got.Name != "doc" {
		t.Fatalf("set field result: %+v", got)
	}
}

func TestTreeMove(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	e := newTreeEngine(store)
	notifier := &recordingNotifier{}
	e.notify = notifier.record
	kb := place(t, e, folderType, "", "kb")
	archive := place(t, e, folderType, kb.ID, "archive")
	doc := place(t, e, documentType, kb.ID, "doc")
	if _, err := e.Publish(ctx, ContentItemMatch{ID: doc.ID}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	notifier.changes = nil

	moved, err := e.Move(ctx, MoveInput{Match: ContentItemMatch{ID: doc.ID}, Name: "renamed"})
	if err != nil || moved.ParentID != kb.ID || moved.Name != "renamed" {
		t.Fatalf("rename: item=%+v err=%v", moved, err)
	}
	moved, err = e.Move(ctx, MoveInput{Match: ContentItemMatch{ID: doc.ID}, ParentID: new(archive.ID)})
	if err != nil || moved.ParentID != archive.ID || moved.Name != "renamed" {
		t.Fatalf("move: item=%+v err=%v", moved, err)
	}
	moved, err = e.Move(ctx, MoveInput{Match: ContentItemMatch{ID: doc.ID}, ParentID: new(""), Name: "top"})
	if err != nil || moved.ParentID != "" || moved.Name != "top" {
		t.Fatalf("move to root: item=%+v err=%v", moved, err)
	}
	if _, err := e.Move(ctx, MoveInput{Match: ContentItemMatch{ID: doc.ID}, ParentID: new(""), Name: "top"}); err != nil {
		t.Fatalf("no-op move: %v", err)
	}

	got := getItem(t, e, doc.ID)
	if got.Draft.Revision != 1 || got.Published == nil || got.State() != StatePublished {
		t.Fatalf("moves must not touch the snapshots: revision=%d state=%s", got.Draft.Revision, got.State())
	}
	if len(notifier.changes) != 3 {
		t.Fatalf("expected 3 move events (no-op emits nothing), got %+v", notifier.changes)
	}
	for _, c := range notifier.changes {
		if c.Op != OpMove || c.Type != documentType || c.ID != doc.ID {
			t.Fatalf("unexpected change %+v", c)
		}
	}
}

func TestTreeMoveRejects(t *testing.T) {
	ctx := context.Background()
	e := newTreeEngine(newFakeStore())
	kb := place(t, e, folderType, "", "kb")
	sub := place(t, e, folderType, kb.ID, "sub")
	subsub := place(t, e, folderType, sub.ID, "subsub")
	doc := place(t, e, documentType, kb.ID, "doc")
	place(t, e, documentType, sub.ID, "taken")
	old := place(t, e, folderType, kb.ID, "old")
	archivedDoc := place(t, e, documentType, kb.ID, "archived-doc")
	for _, id := range []string{archivedDoc.ID, old.ID} {
		if _, err := e.Archive(ctx, ContentItemMatch{ID: id}); err != nil {
			t.Fatalf("archive: %v", err)
		}
	}
	legacy, err := e.SaveDraft(ctx, SaveDraftInput{Type: testType, Fields: validFields()})
	if err != nil {
		t.Fatalf("legacy item: %v", err)
	}

	tests := []struct {
		name  string
		input MoveInput
		kind  Kind
	}{
		{"empty match", MoveInput{Name: "x"}, KindBadRequest},
		{"missing item", MoveInput{Match: ContentItemMatch{ID: "nope"}, Name: "x"}, KindNotFound},
		{"into itself", MoveInput{Match: ContentItemMatch{ID: sub.ID}, ParentID: new(sub.ID)}, KindBadRequest},
		{"into its child", MoveInput{Match: ContentItemMatch{ID: kb.ID}, ParentID: new(sub.ID)}, KindBadRequest},
		{"into its grandchild", MoveInput{Match: ContentItemMatch{ID: kb.ID}, ParentID: new(subsub.ID)}, KindBadRequest},
		{"under a document", MoveInput{Match: ContentItemMatch{ID: sub.ID}, ParentID: new(doc.ID)}, KindBadRequest},
		{"under a missing parent", MoveInput{Match: ContentItemMatch{ID: doc.ID}, ParentID: new("nope")}, KindBadRequest},
		{"under an archived parent", MoveInput{Match: ContentItemMatch{ID: doc.ID}, ParentID: new(old.ID)}, KindNotAcceptable},
		{"an archived item", MoveInput{Match: ContentItemMatch{ID: archivedDoc.ID}, Name: "x"}, KindNotAcceptable},
		{"to a taken name", MoveInput{Match: ContentItemMatch{ID: doc.ID}, ParentID: new(sub.ID), Name: "taken"}, KindConflict},
		{"to an invalid name", MoveInput{Match: ContentItemMatch{ID: doc.ID}, Name: "a/b"}, KindBadRequest},
		{"a non-tree item", MoveInput{Match: ContentItemMatch{ID: legacy.ID}, Name: "x"}, KindBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := e.Move(ctx, tt.input)
			wantKind(t, err, tt.kind)
		})
	}
	if got := getItem(t, e, kb.ID); got.ParentID != "" || got.Name != "kb" {
		t.Fatalf("rejected moves must not write: %+v", got)
	}
}

func TestTreeDeleteRequiresNoChildren(t *testing.T) {
	ctx := context.Background()
	e := newTreeEngine(newFakeStore())
	kb := place(t, e, folderType, "", "kb")
	doc := place(t, e, documentType, kb.ID, "doc")

	_, err := e.Delete(ctx, ContentItemMatch{ID: kb.ID})
	wantKind(t, err, KindNotAcceptable)
	if _, err := e.Archive(ctx, ContentItemMatch{ID: doc.ID}); err != nil {
		t.Fatalf("archive child: %v", err)
	}
	_, err = e.Delete(ctx, ContentItemMatch{ID: kb.ID})
	wantKind(t, err, KindNotAcceptable) // archived children still block a delete

	if deleted, err := e.Delete(ctx, ContentItemMatch{ID: doc.ID}); err != nil || !deleted {
		t.Fatalf("delete leaf: deleted=%v err=%v", deleted, err)
	}
	if deleted, err := e.Delete(ctx, ContentItemMatch{ID: kb.ID}); err != nil || !deleted {
		t.Fatalf("delete empty folder: deleted=%v err=%v", deleted, err)
	}
	if deleted, err := e.Delete(ctx, ContentItemMatch{ID: kb.ID}); err != nil || deleted {
		t.Fatalf("delete missing: deleted=%v err=%v", deleted, err)
	}
}

func TestTreeArchiveKeepsArchivedAncestorsConsistent(t *testing.T) {
	ctx := context.Background()
	e := newTreeEngine(newFakeStore())
	kb := place(t, e, folderType, "", "kb")
	doc := place(t, e, documentType, kb.ID, "doc")

	_, err := e.Archive(ctx, ContentItemMatch{ID: kb.ID})
	wantKind(t, err, KindNotAcceptable)
	for _, id := range []string{doc.ID, kb.ID} {
		if _, err := e.Archive(ctx, ContentItemMatch{ID: id}); err != nil {
			t.Fatalf("archive %s: %v", id, err)
		}
	}

	_, err = e.Unarchive(ctx, ContentItemMatch{ID: doc.ID})
	wantKind(t, err, KindNotAcceptable)
	for _, id := range []string{kb.ID, doc.ID} {
		if _, err := e.Unarchive(ctx, ContentItemMatch{ID: id}); err != nil {
			t.Fatalf("unarchive %s: %v", id, err)
		}
	}
}

func TestTreeLifecycleRunsInTreeTransactions(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	e := newTreeEngine(store)
	doc := place(t, e, documentType, "", "doc")
	match := ContentItemMatch{ID: doc.ID}
	steps := []func() error{
		func() error {
			_, err := e.SaveDraft(ctx, SaveDraftInput{Match: match, Fields: titleFields("v2")})
			return err
		},
		func() error { _, err := e.Publish(ctx, match); return err },
		func() error { _, err := e.Unpublish(ctx, match); return err },
		func() error { _, err := e.Unpublish(ctx, match); return err },
		func() error { _, err := e.Archive(ctx, match); return err },
		func() error { _, err := e.Unarchive(ctx, match); return err },
		func() error { _, err := e.Delete(ctx, match); return err },
	}
	for i, step := range steps {
		before := store.treeTransactions
		if err := step(); err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
		if store.treeTransactions != before+1 {
			t.Fatalf("step %d did not run in a tree transaction", i)
		}
	}
}

// storeOnly hides the fake store's TreeStore methods.
type storeOnly struct{ Store }

func TestTreeRequiresTreeStore(t *testing.T) {
	ctx := context.Background()
	e := newTreeEngine(storeOnly{newFakeStore()})
	_, err := e.SaveDraft(ctx, SaveDraftInput{Type: folderType, Name: "kb", Fields: ContentFields{}})
	wantKind(t, err, KindInternal)
	_, err = e.Move(ctx, MoveInput{Match: ContentItemMatch{ID: "x"}})
	wantKind(t, err, KindInternal)
	if _, err := e.SaveDraft(ctx, SaveDraftInput{Type: testType, Fields: validFields()}); err != nil {
		t.Fatalf("non-tree types must keep working without a TreeStore: %v", err)
	}
}

// TestTreePlacedItemsStayGuarded covers a schema release that drops tree types: items
// that already carry a name keep taking the guarded tree path.
func TestTreePlacedItemsStayGuarded(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	setup := newTreeEngine(store)
	kb := place(t, setup, folderType, "", "kb")
	place(t, setup, documentType, kb.ID, "doc")

	reg := testRegistry()
	reg.Register(NewSchema(folderType, "2", []FieldDefinition{{ID: "title", Type: FieldTypeSymbol}}))
	e := New(Deps{Store: store, Schemas: reg, Locales: testLocales, NewID: func() string { return "unused" }})
	before := store.treeTransactions
	_, err := e.Delete(ctx, ContentItemMatch{ID: kb.ID})
	wantKind(t, err, KindNotAcceptable)
	if store.treeTransactions != before+1 {
		t.Fatal("a placed item must take the tree path")
	}
}
