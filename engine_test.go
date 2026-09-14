package contentstore

import (
	"context"
	"errors"
	"testing"
)

func TestPublishEmitsChange(t *testing.T) {
	n := &recordingNotifier{}
	e := New(Deps{Store: newFakeStore(), Schemas: testRegistry(), Locales: testLocales, NewID: testNewID(), Notify: n.record})
	ctx := context.Background()
	key := "contentful:shopCategory:cat-9:seoOverride:brand=puma"
	seedPublishable(t, e, key, "cat-9")

	if _, err := e.Publish(ctx, ContentItemMatch{Key: key}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if len(n.changes) != 1 {
		t.Fatalf("expected exactly one change, got %#v", n.changes)
	}
	ch := n.changes[0]
	if ch.Op != OpPublish || ch.Type != testType || ch.OwnerRef == nil || ch.OwnerRef.ID != "cat-9" {
		t.Fatalf("unexpected change %#v", ch)
	}
}

func TestDraftWritesEmitChanges(t *testing.T) {
	n := &recordingNotifier{}
	e := New(Deps{Store: newFakeStore(), Schemas: testRegistry(), Locales: testLocales, NewID: testNewID(), Notify: n.record})
	ctx := context.Background()
	key := "contentful:shopCategory:cat-9:seoOverride:brand=puma"

	if _, err := e.SaveDraft(ctx, SaveDraftInput{
		Match:    ContentItemMatch{Key: key},
		Type:     testType,
		OwnerRef: &OwnerRef{Source: "contentful", Type: "shopCategory", ID: "cat-9"},
		Fields:   ContentFields{"filterSelection": {"de": map[string]any{"brand": "puma"}}},
	}); err != nil {
		t.Fatalf("save draft: %v", err)
	}
	if _, err := e.SetFieldValue(ctx, SetFieldValueInput{
		Match: ContentItemMatch{Key: key}, FieldID: "metaTitle", Locale: "de", Value: "Puma",
	}); err != nil {
		t.Fatalf("set field value: %v", err)
	}

	if len(n.changes) != 2 {
		t.Fatalf("expected one change per draft write, got %#v", n.changes)
	}
	for _, change := range n.changes {
		if change.Op != OpSaveDraft || change.Type != testType || change.OwnerRef == nil || change.OwnerRef.ID != "cat-9" {
			t.Fatalf("unexpected change %#v", change)
		}
	}
}

func TestPublishWithoutNotifierIsNilSafe(t *testing.T) {
	e := newTestEngine(newFakeStore()) // nil notifier
	ctx := context.Background()
	key := "contentful:shopCategory:cat-1:seoOverride:brand=puma"
	seedPublishable(t, e, key, "cat-1")
	if _, err := e.Publish(ctx, ContentItemMatch{Key: key}); err != nil {
		t.Fatalf("publish must succeed with nil notifier: %v", err)
	}
}

func TestSuppressChangeEventsSilencesNotifier(t *testing.T) {
	const silentType ContentType = "silentDraft"
	reg := testRegistry()
	s := NewSchema(silentType, "1", []FieldDefinition{{ID: "payload", Type: FieldTypeJSON, Localized: false}})
	s.SuppressChangeEvents = true
	reg.Register(s)

	n := &recordingNotifier{}
	e := New(Deps{Store: newFakeStore(), Schemas: reg, Locales: testLocales, NewID: testNewID(), Notify: n.record})
	ctx := context.Background()
	key := "contentstudio:draft:job-1:entry-1:de"

	if _, err := e.SaveDraft(ctx, SaveDraftInput{
		Match:    ContentItemMatch{Key: key},
		Type:     silentType,
		OwnerRef: &OwnerRef{Source: "contentstudio", Type: "job", ID: "job-1"},
		Fields:   ContentFields{"payload": {"de": map[string]any{"k": "v"}}},
	}); err != nil {
		t.Fatalf("save draft: %v", err)
	}
	if _, err := e.Publish(ctx, ContentItemMatch{Key: key}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if _, err := e.Delete(ctx, ContentItemMatch{Key: key}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if len(n.changes) != 0 {
		t.Fatalf("suppressed type must not notify, got %#v", n.changes)
	}

	// Sanity: a normal type on the same engine still notifies, so suppression is
	// per-type, not a broken notifier.
	seedPublishable(t, e, "contentful:shopCategory:cat-1:seoOverride:brand=puma", "cat-1")
	if _, err := e.Publish(ctx, ContentItemMatch{Key: "contentful:shopCategory:cat-1:seoOverride:brand=puma"}); err != nil {
		t.Fatalf("publish normal: %v", err)
	}
	if len(n.changes) != 1 || n.changes[0].Type != testType {
		t.Fatalf("normal type must notify exactly once, got %#v", n.changes)
	}
}

func TestGetSnapshotSelectsPublishedAndPreviewRepresentations(t *testing.T) {
	e := newTestEngine(newFakeStore())
	ctx := context.Background()

	seedPublishable(t, e, "k-draft", "cat-1")
	seedPublishable(t, e, "k-published", "cat-2")
	if _, err := e.Publish(ctx, ContentItemMatch{Key: "k-published"}); err != nil {
		t.Fatalf("publish k-published: %v", err)
	}
	seedPublishable(t, e, "k-changed", "cat-3")
	if _, err := e.SetFieldValue(ctx, SetFieldValueInput{
		Match: ContentItemMatch{Key: "k-changed"}, FieldID: "metaTitle", Locale: "de", Value: "published value",
	}); err != nil {
		t.Fatalf("seed k-changed title: %v", err)
	}
	if _, err := e.Publish(ctx, ContentItemMatch{Key: "k-changed"}); err != nil {
		t.Fatalf("publish k-changed: %v", err)
	}
	if _, err := e.SetFieldValue(ctx, SetFieldValueInput{
		Match: ContentItemMatch{Key: "k-changed"}, FieldID: "metaTitle", Locale: "de", Value: "edited draft",
	}); err != nil {
		t.Fatalf("edit k-changed: %v", err)
	}
	seedPublishable(t, e, "k-archived", "cat-4")
	if _, err := e.Publish(ctx, ContentItemMatch{Key: "k-archived"}); err != nil {
		t.Fatalf("publish k-archived: %v", err)
	}
	if _, err := e.Archive(ctx, ContentItemMatch{Key: "k-archived"}); err != nil {
		t.Fatalf("archive k-archived: %v", err)
	}

	published, err := e.GetSnapshot(ctx, SnapshotViewPublished, SnapshotInput{Types: []ContentType{testType}})
	if err != nil {
		t.Fatalf("GetSnapshot published: %v", err)
	}
	publishedByKey := make(map[string]SnapshotItem, len(published.Items))
	for _, item := range published.Items {
		publishedByKey[item.Key] = item
	}
	if len(publishedByKey) != 2 {
		t.Fatalf("published view returned %#v, want published and changed items", published.Items)
	}
	if got := publishedByKey["k-changed"]; got.State != StatePublished || got.Fields["metaTitle"]["de"] != "published value" {
		t.Fatalf("published view leaked working draft: %#v", got)
	}
	if _, ok := publishedByKey["k-draft"]; ok {
		t.Fatal("published view must exclude draft-only items")
	}
	if _, ok := publishedByKey["k-archived"]; ok {
		t.Fatal("published view must exclude archived items")
	}

	preview, err := e.GetSnapshot(ctx, SnapshotViewPreview, SnapshotInput{Types: []ContentType{testType}})
	if err != nil {
		t.Fatalf("GetSnapshot preview: %v", err)
	}
	previewByKey := make(map[string]SnapshotItem, len(preview.Items))
	for _, item := range preview.Items {
		previewByKey[item.Key] = item
	}
	if len(previewByKey) != 3 {
		t.Fatalf("preview view returned %#v, want all non-archived items", preview.Items)
	}
	if got := previewByKey["k-draft"]; got.State != StateDraft {
		t.Fatalf("draft-only preview state = %q, want %q", got.State, StateDraft)
	}
	if got := previewByKey["k-published"]; got.State != StatePublished {
		t.Fatalf("published preview state = %q, want %q", got.State, StatePublished)
	}
	if got := previewByKey["k-changed"]; got.State != StateChanged || got.Fields["metaTitle"]["de"] != "edited draft" {
		t.Fatalf("preview view did not return working draft: %#v", got)
	}
	if _, ok := previewByKey["k-archived"]; ok {
		t.Fatal("preview view must exclude archived items")
	}

	owner := &OwnerRef{Source: "contentful", Type: "shopCategory", ID: "cat-3"}
	for _, view := range []SnapshotView{SnapshotViewPublished, SnapshotViewPreview} {
		owned, err := e.GetSnapshot(ctx, view, SnapshotInput{OwnerRef: owner})
		if err != nil {
			t.Fatalf("GetSnapshot %s by owner: %v", view, err)
		}
		if len(owned.Items) != 1 || owned.Items[0].Key != "k-changed" {
			t.Fatalf("GetSnapshot %s owner filter returned %#v", view, owned.Items)
		}
	}
}

func TestGetSnapshotRejectsUnknownView(t *testing.T) {
	e := newTestEngine(newFakeStore())
	_, err := e.GetSnapshot(context.Background(), SnapshotView("unknown"), SnapshotInput{})
	if err == nil {
		t.Fatal("expected an unsupported view error")
	}
	wantKind(t, err, KindBadRequest)
}

func TestUnarchiveReturnsPublishedItemToDraft(t *testing.T) {
	e := newTestEngine(newFakeStore())
	ctx := context.Background()
	key := "contentful:shopCategory:cat-9:seoOverride:brand=puma"

	seedPublishable(t, e, key, "cat-9")
	if _, err := e.Publish(ctx, ContentItemMatch{Key: key}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if _, err := e.Archive(ctx, ContentItemMatch{Key: key}); err != nil {
		t.Fatalf("archive: %v", err)
	}

	un, err := e.Unarchive(ctx, ContentItemMatch{Key: key})
	if err != nil {
		t.Fatalf("unarchive: %v", err)
	}
	if un.ArchivedAt != nil {
		t.Fatalf("unarchive must clear ArchivedAt, got %v", un.ArchivedAt)
	}
	if un.Published != nil {
		t.Fatalf("unarchive must drop the published snapshot, got %#v", un.Published)
	}
	if un.Draft == nil {
		t.Fatalf("unarchive must preserve the working draft")
	}
	if un.State() != StateDraft {
		t.Fatalf("expected draft state after unarchive, got %s", un.State())
	}

	// No longer published until re-published.
	snap, err := e.GetSnapshot(ctx, SnapshotViewPublished, SnapshotInput{})
	if err != nil {
		t.Fatalf("published snapshot: %v", err)
	}
	if len(snap.Items) != 0 {
		t.Fatalf("expected item excluded from published snapshot, got %#v", snap.Items)
	}
}

func TestUnpublishReturnsItemToDraftAndKeepsWorkingCopy(t *testing.T) {
	n := &recordingNotifier{}
	e := New(Deps{Store: newFakeStore(), Schemas: testRegistry(), Locales: testLocales, NewID: testNewID(), Notify: n.record})
	ctx := context.Background()
	key := "contentful:shopCategory:cat-9:seoOverride:brand=puma"

	seedPublishable(t, e, key, "cat-9")
	if _, err := e.Publish(ctx, ContentItemMatch{Key: key}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	un, err := e.Unpublish(ctx, ContentItemMatch{Key: key})
	if err != nil {
		t.Fatalf("unpublish: %v", err)
	}
	if un.Published != nil {
		t.Fatalf("unpublish must drop the published snapshot, got %#v", un.Published)
	}
	if un.ArchivedAt != nil {
		t.Fatalf("unpublish must not archive the item, got %v", un.ArchivedAt)
	}
	if un.Draft == nil {
		t.Fatalf("unpublish must preserve the working draft")
	}
	if un.State() != StateDraft {
		t.Fatalf("expected draft state after unpublish, got %s", un.State())
	}

	published, err := e.GetSnapshot(ctx, SnapshotViewPublished, SnapshotInput{})
	if err != nil {
		t.Fatalf("published snapshot: %v", err)
	}
	if len(published.Items) != 0 {
		t.Fatalf("expected item excluded from published snapshot, got %#v", published.Items)
	}
	// Still editorially visible: unpublish takes content off the site, it does not hide it.
	preview, err := e.GetSnapshot(ctx, SnapshotViewPreview, SnapshotInput{})
	if err != nil {
		t.Fatalf("preview snapshot: %v", err)
	}
	if len(preview.Items) != 1 || preview.Items[0].Key != key {
		t.Fatalf("expected item in preview snapshot, got %#v", preview.Items)
	}

	if len(n.changes) != 2 { // publish + unpublish
		t.Fatalf("expected publish and unpublish changes, got %#v", n.changes)
	}
	if ch := n.changes[1]; ch.Op != OpUnpublish || ch.Type != testType || ch.OwnerRef == nil || ch.OwnerRef.ID != "cat-9" {
		t.Fatalf("unexpected unpublish change %#v", ch)
	}
}

func TestUnpublishNeverPublishedItemIsASilentNoOp(t *testing.T) {
	n := &recordingNotifier{}
	e := New(Deps{Store: newFakeStore(), Schemas: testRegistry(), Locales: testLocales, NewID: testNewID(), Notify: n.record})
	ctx := context.Background()
	key := "contentful:shopCategory:cat-9:seoOverride:brand=puma"
	seedPublishable(t, e, key, "cat-9")

	// A bulk unpublish over a mixed selection must not fail on rows already in the
	// target state, and an unchanged item must not invalidate consumer caches.
	item, err := e.Unpublish(ctx, ContentItemMatch{Key: key})
	if err != nil {
		t.Fatalf("unpublish of a draft-only item must succeed: %v", err)
	}
	if item.State() != StateDraft || item.Draft == nil {
		t.Fatalf("expected an untouched draft item, got %#v", item)
	}
	if len(n.changes) != 0 {
		t.Fatalf("a no-op unpublish must not emit, got %#v", n.changes)
	}
}

func TestUnpublishMissingItem(t *testing.T) {
	e := newTestEngine(newFakeStore())
	_, err := e.Unpublish(context.Background(), ContentItemMatch{Key: "nope"})
	if err == nil {
		t.Fatal("expected a not-found error")
	}
	wantKind(t, err, KindNotFound)
}

// An archived item is frozen: it may only be unarchived, deleted, or read. Without this
// an archived item could take draft writes, or be published into a snapshot that
// Unarchive would then silently discard.
func TestArchivedItemRejectsWrites(t *testing.T) {
	key := "contentful:shopCategory:cat-9:seoOverride:brand=puma"
	writes := map[string]func(*Engine) error{
		"publish": func(e *Engine) error {
			_, err := e.Publish(context.Background(), ContentItemMatch{Key: key})
			return err
		},
		"unpublish": func(e *Engine) error {
			_, err := e.Unpublish(context.Background(), ContentItemMatch{Key: key})
			return err
		},
		"saveDraft": func(e *Engine) error {
			_, err := e.SaveDraft(context.Background(), SaveDraftInput{Match: ContentItemMatch{Key: key}, Fields: validFields()})
			return err
		},
		"setFieldValue": func(e *Engine) error {
			_, err := e.SetFieldValue(context.Background(), SetFieldValueInput{
				Match: ContentItemMatch{Key: key}, FieldID: "metaTitle", Locale: "de", Value: "Puma",
			})
			return err
		},
	}
	for name, write := range writes {
		t.Run(name, func(t *testing.T) {
			e := newTestEngine(newFakeStore())
			seedPublishable(t, e, key, "cat-9")
			if _, err := e.Publish(context.Background(), ContentItemMatch{Key: key}); err != nil {
				t.Fatalf("publish: %v", err)
			}
			if _, err := e.Archive(context.Background(), ContentItemMatch{Key: key}); err != nil {
				t.Fatalf("archive: %v", err)
			}
			err := write(e)
			if err == nil {
				t.Fatalf("%s must be rejected on an archived item", name)
			}
			wantKind(t, err, KindNotAcceptable)
		})
	}
}

// Unarchive and Delete stay available, otherwise archiving would be a one-way trapdoor.
func TestArchivedItemAllowsUnarchiveAndDelete(t *testing.T) {
	ctx := context.Background()
	for _, name := range []string{"unarchive", "delete"} {
		t.Run(name, func(t *testing.T) {
			e := newTestEngine(newFakeStore())
			key := "contentful:shopCategory:cat-9:seoOverride:brand=puma"
			seedPublishable(t, e, key, "cat-9")
			if _, err := e.Archive(ctx, ContentItemMatch{Key: key}); err != nil {
				t.Fatalf("archive: %v", err)
			}
			var err error
			if name == "unarchive" {
				_, err = e.Unarchive(ctx, ContentItemMatch{Key: key})
			} else {
				_, err = e.Delete(ctx, ContentItemMatch{Key: key})
			}
			if err != nil {
				t.Fatalf("%s of an archived item must succeed: %v", name, err)
			}
		})
	}
}

func TestGetArchivedSnapshot(t *testing.T) {
	e := newTestEngine(newFakeStore())
	ctx := context.Background()

	// Two archived items and one live one; only the archived items must appear.
	seedPublishable(t, e, "k-arch-1", "cat-1")
	if _, err := e.Archive(ctx, ContentItemMatch{Key: "k-arch-1"}); err != nil {
		t.Fatalf("archive k-arch-1: %v", err)
	}
	seedPublishable(t, e, "k-arch-2", "cat-2")
	if _, err := e.Archive(ctx, ContentItemMatch{Key: "k-arch-2"}); err != nil {
		t.Fatalf("archive k-arch-2: %v", err)
	}
	seedPublishable(t, e, "k-live", "cat-3") // not archived

	snap, err := e.GetArchivedSnapshot(ctx, ArchivedSnapshotInput{Types: []ContentType{testType}})
	if err != nil {
		t.Fatalf("GetArchivedSnapshot: %v", err)
	}
	got := map[string]ArchivedContentItem{}
	for _, it := range snap.Items {
		got[it.Key] = it
	}
	if len(got) != 2 {
		t.Fatalf("expected exactly the 2 archived items, got %#v", snap.Items)
	}
	for _, key := range []string{"k-arch-1", "k-arch-2"} {
		it, ok := got[key]
		if !ok {
			t.Fatalf("archived item %q missing", key)
		}
		if it.ArchivedAt.IsZero() {
			t.Fatalf("archived item %q must carry ArchivedAt", key)
		}
		if it.Fields["filterSelection"] == nil {
			t.Fatalf("archived item %q must carry draft fields", key)
		}
	}
	if _, ok := got["k-live"]; ok {
		t.Fatalf("non-archived item must be excluded, got %#v", snap.Items)
	}
}

func TestSaveDraftPublishLifecycle(t *testing.T) {
	e := newTestEngine(newFakeStore())
	ctx := context.Background()

	// Create.
	item, err := e.SaveDraft(ctx, SaveDraftInput{Type: testType, Fields: validFields()})
	if err != nil {
		t.Fatalf("create draft: %v", err)
	}
	if item.State() != StateDraft || item.ID == "" || item.Draft.Revision != 1 {
		t.Fatalf("unexpected create result: state=%s item=%#v", item.State(), item)
	}
	id := item.ID

	// Update draft by ID → revision bumps, still draft.
	item, err = e.SaveDraft(ctx, SaveDraftInput{Match: ContentItemMatch{ID: id}, Fields: validFields()})
	if err != nil {
		t.Fatalf("update draft: %v", err)
	}
	if item.State() != StateDraft || item.Draft.Revision != 2 {
		t.Fatalf("expected draft rev 2, got state=%s rev=%d", item.State(), item.Draft.Revision)
	}

	// Publish → published, snapshots at equal revision.
	pub, err := e.Publish(ctx, ContentItemMatch{ID: id})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if pub.State() != StatePublished || pub.Published == nil {
		t.Fatalf("expected published, got %s", pub.State())
	}

	// Save again → Changed (draft revision ahead of published).
	item, err = e.SaveDraft(ctx, SaveDraftInput{Match: ContentItemMatch{ID: id}, Fields: validFields()})
	if err != nil {
		t.Fatalf("change draft: %v", err)
	}
	if item.State() != StateChanged {
		t.Fatalf("expected changed, got %s", item.State())
	}

	// Archive → hidden; then unarchive → returns to Draft (a previously-published item is
	// NOT auto-republished; symmetric to Contentful). The working draft is preserved.
	if ar, err := e.Archive(ctx, ContentItemMatch{ID: id}); err != nil || ar.State() != StateArchived {
		t.Fatalf("archive: state=%v err=%v", ar, err)
	}
	un, err := e.Unarchive(ctx, ContentItemMatch{ID: id})
	if err != nil {
		t.Fatalf("unarchive: %v", err)
	}
	if un.State() != StateDraft || un.Published != nil || un.Draft == nil {
		t.Fatalf("expected draft (published dropped, draft kept) after unarchive, got state=%s published=%v", un.State(), un.Published)
	}

	// The unarchived item is no longer published, so the published snapshot is empty
	// until it is re-published.
	snap, err := e.GetSnapshot(ctx, SnapshotViewPublished, SnapshotInput{})
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(snap.Items) != 0 {
		t.Fatalf("expected 0 published items after unarchive, got %#v", snap.Items)
	}

	// Delete is idempotent.
	if deleted, err := e.Delete(ctx, ContentItemMatch{ID: id}); err != nil || !deleted {
		t.Fatalf("delete: deleted=%v err=%v", deleted, err)
	}
	if deleted, err := e.Delete(ctx, ContentItemMatch{ID: id}); err != nil || deleted {
		t.Fatalf("second delete must be idempotent (false), got deleted=%v err=%v", deleted, err)
	}
}

func TestPublishRequiresFilterSelection(t *testing.T) {
	e := newTestEngine(newFakeStore())
	ctx := context.Background()

	item, err := e.SaveDraft(ctx, SaveDraftInput{
		Type:   testType,
		Fields: mkFields(map[string]map[string]any{"metaTitle": {"de": "x"}}), // no filterSelection
	})
	if err != nil {
		t.Fatalf("save draft: %v", err)
	}
	_, err = e.Publish(ctx, ContentItemMatch{ID: item.ID})
	if err == nil {
		t.Fatal("expected error publishing without filterSelection")
	}
	wantKind(t, err, KindNotAcceptable)
}

func TestSaveDraftUpsertByKey(t *testing.T) {
	e := newTestEngine(newFakeStore())
	ctx := context.Background()
	key := "contentful:shopCategory:cat-1:seoOverride:brand=nike"

	item, err := e.SaveDraft(ctx, SaveDraftInput{Match: ContentItemMatch{Key: key}, Type: testType, Fields: validFields()})
	if err != nil {
		t.Fatalf("create by key: %v", err)
	}
	if item.Key != key {
		t.Fatalf("expected key %q, got %q", key, item.Key)
	}
	id := item.ID

	// Second save by the same key must hit the same item, not create a new one.
	item, err = e.SaveDraft(ctx, SaveDraftInput{Match: ContentItemMatch{Key: key}, Fields: validFields()})
	if err != nil {
		t.Fatalf("update by key: %v", err)
	}
	if item.ID != id || item.Draft.Revision != 2 {
		t.Fatalf("expected same item at rev 2, got id=%s rev=%d", item.ID, item.Draft.Revision)
	}
}

func TestSaveDraftExpectedRevision(t *testing.T) {
	e := newTestEngine(newFakeStore())
	ctx := context.Background()
	key := "contentful:shopCategory:cat-1:seoOverride:brand=nike"
	createOnly := int64(0)

	created, err := e.SaveDraft(ctx, SaveDraftInput{
		Match: ContentItemMatch{Key: key}, Type: testType,
		Fields: validFields(), ExpectedRevision: &createOnly,
	})
	if err != nil {
		t.Fatalf("create-only save: %v", err)
	}
	if created.Draft.Revision != 1 {
		t.Fatalf("create-only revision = %d, want 1", created.Draft.Revision)
	}

	if _, err = e.SaveDraft(ctx, SaveDraftInput{
		Match: ContentItemMatch{Key: key}, Fields: validFields(), ExpectedRevision: &createOnly,
	}); err == nil {
		t.Fatal("second create-only save must conflict")
	} else {
		wantKind(t, err, KindConflict)
	}

	revisionOne := int64(1)
	updatedFields := validFields()
	updatedFields["metaTitle"]["de"] = "Updated"
	updated, err := e.SaveDraft(ctx, SaveDraftInput{
		Match: ContentItemMatch{Key: key}, Fields: updatedFields, ExpectedRevision: &revisionOne,
	})
	if err != nil {
		t.Fatalf("conditional update: %v", err)
	}
	if updated.Draft.Revision != 2 || updated.Draft.Fields["metaTitle"]["de"] != "Updated" {
		t.Fatalf("unexpected conditional update: %#v", updated.Draft)
	}

	staleFields := validFields()
	staleFields["metaTitle"]["de"] = "Stale"
	if _, err = e.SaveDraft(ctx, SaveDraftInput{
		Match: ContentItemMatch{Key: key}, Fields: staleFields, ExpectedRevision: &revisionOne,
	}); err == nil {
		t.Fatal("stale conditional update must conflict")
	} else {
		wantKind(t, err, KindConflict)
	}
	got, err := e.GetItem(ctx, ContentItemMatch{Key: key})
	if err != nil {
		t.Fatalf("get after stale update: %v", err)
	}
	if got.Draft.Revision != 2 || got.Draft.Fields["metaTitle"]["de"] != "Updated" {
		t.Fatalf("stale update modified the draft: %#v", got.Draft)
	}
}

func TestSaveDraftExpectedRevisionRejectsNegativeValue(t *testing.T) {
	e := newTestEngine(newFakeStore())
	negative := int64(-1)
	if _, err := e.SaveDraft(context.Background(), SaveDraftInput{
		Type: testType, Fields: validFields(), ExpectedRevision: &negative,
	}); err == nil {
		t.Fatal("negative expected revision must fail")
	} else {
		wantKind(t, err, KindBadRequest)
	}
}

func TestSaveDraftExpectedRevisionAllowsOneConcurrentWinner(t *testing.T) {
	e := newTestEngine(newFakeStore())
	ctx := context.Background()
	key := "contentful:shopCategory:cat-1:seoOverride:brand=nike"
	if _, err := e.SaveDraft(ctx, SaveDraftInput{
		Match: ContentItemMatch{Key: key}, Type: testType, Fields: validFields(),
	}); err != nil {
		t.Fatalf("seed draft: %v", err)
	}

	expected := int64(1)
	start := make(chan struct{})
	errs := make(chan error, 2)
	for _, title := range []string{"First", "Second"} {
		go func() {
			fields := validFields()
			fields["metaTitle"]["de"] = title
			<-start
			_, err := e.SaveDraft(ctx, SaveDraftInput{
				Match: ContentItemMatch{Key: key}, Fields: fields, ExpectedRevision: &expected,
			})
			errs <- err
		}()
	}
	close(start)

	succeeded, conflicted := 0, 0
	for range 2 {
		err := <-errs
		if err == nil {
			succeeded++
			continue
		}
		var contentErr *Error
		if errors.As(err, &contentErr) && contentErr.Kind == KindConflict {
			conflicted++
			continue
		}
		t.Fatalf("unexpected concurrent-save error: %v", err)
	}
	if succeeded != 1 || conflicted != 1 {
		t.Fatalf("concurrent results: succeeded=%d conflicted=%d", succeeded, conflicted)
	}
	got, err := e.GetItem(ctx, ContentItemMatch{Key: key})
	if err != nil {
		t.Fatalf("get winner: %v", err)
	}
	if got.Draft.Revision != 2 {
		t.Fatalf("winner revision = %d, want 2", got.Draft.Revision)
	}
}

func TestSetFieldValueMergesAndCreates(t *testing.T) {
	e := newTestEngine(newFakeStore())
	ctx := context.Background()
	key := "contentful:shopCategory:cat-9:seoOverride:brand=puma"

	// Create-on-first-set: no item yet; Type + key provided.
	item, err := e.SetFieldValue(ctx, SetFieldValueInput{
		Match:    ContentItemMatch{Key: key},
		Type:     testType,
		OwnerRef: &OwnerRef{Source: "contentful", Type: "shopCategory", ID: "cat-9"},
		FieldID:  "metaTitle", Locale: "de", Value: "Puma",
	})
	if err != nil {
		t.Fatalf("create-set: %v", err)
	}
	id := item.ID
	if item.Draft.Revision != 1 || item.Draft.Fields["metaTitle"]["de"] != "Puma" {
		t.Fatalf("unexpected create-set draft: %#v", item.Draft)
	}

	// Set another field → merge; existing field preserved; revision bumps.
	item, err = e.SetFieldValue(ctx, SetFieldValueInput{Match: ContentItemMatch{ID: id}, FieldID: "metaDescription", Locale: "de", Value: "Puma Schuhe"})
	if err != nil {
		t.Fatalf("merge-set: %v", err)
	}
	if item.Draft.Revision != 2 ||
		item.Draft.Fields["metaTitle"]["de"] != "Puma" ||
		item.Draft.Fields["metaDescription"]["de"] != "Puma Schuhe" {
		t.Fatalf("merge dropped a field: %#v", item.Draft.Fields)
	}

	// Add a second locale to metaTitle → both retained.
	item, _ = e.SetFieldValue(ctx, SetFieldValueInput{Match: ContentItemMatch{ID: id}, FieldID: "metaTitle", Locale: "fr", Value: "Puma FR"})
	if item.Draft.Fields["metaTitle"]["de"] != "Puma" || item.Draft.Fields["metaTitle"]["fr"] != "Puma FR" {
		t.Fatalf("expected both locales, got %#v", item.Draft.Fields["metaTitle"])
	}

	// Clear a locale with nil → removed.
	item, _ = e.SetFieldValue(ctx, SetFieldValueInput{Match: ContentItemMatch{ID: id}, FieldID: "metaTitle", Locale: "fr", Value: nil})
	if _, ok := item.Draft.Fields["metaTitle"]["fr"]; ok {
		t.Fatalf("expected fr cleared, got %#v", item.Draft.Fields["metaTitle"])
	}

	// Generated ReadOnly field is settable at the storage layer (ReadOnly is enforced by
	// the transport/UI, not the engine).
	if _, err = e.SetFieldValue(ctx, SetFieldValueInput{Match: ContentItemMatch{ID: id}, FieldID: "filterSelection", Locale: "de", Value: map[string]any{"brand": "puma"}}); err != nil {
		t.Fatalf("filterSelection must be settable at the storage layer: %v", err)
	}

	// Unknown field → BadRequest.
	if _, err = e.SetFieldValue(ctx, SetFieldValueInput{Match: ContentItemMatch{ID: id}, FieldID: "nope", Locale: "de", Value: "x"}); err == nil {
		t.Fatal("expected error for unknown field")
	} else {
		wantKind(t, err, KindBadRequest)
	}
	// Missing fieldID → BadRequest.
	if _, err = e.SetFieldValue(ctx, SetFieldValueInput{Match: ContentItemMatch{ID: id}, FieldID: "", Locale: "de", Value: "x"}); err == nil {
		t.Fatal("expected error for empty fieldID")
	}
}

func TestSaveDraftRejectsNilFieldsAndDoesNotWipe(t *testing.T) {
	e := newTestEngine(newFakeStore())
	ctx := context.Background()

	item, err := e.SaveDraft(ctx, SaveDraftInput{Type: testType, Fields: validFields()})
	if err != nil {
		t.Fatalf("seed draft: %v", err)
	}
	id := item.ID

	// A SaveDraft with no fields (nil) must be rejected, not applied.
	if _, err = e.SaveDraft(ctx, SaveDraftInput{Match: ContentItemMatch{ID: id}}); err == nil {
		t.Fatal("expected error for nil fields")
	} else {
		wantKind(t, err, KindBadRequest)
	}

	// The existing draft must be untouched (no revision bump, fields intact).
	got, err := e.GetItem(ctx, ContentItemMatch{ID: id})
	if err != nil {
		t.Fatalf("get item: %v", err)
	}
	if got.Draft.Revision != 1 || got.Draft.Fields == nil {
		t.Fatalf("existing draft must be untouched, got %#v", got.Draft)
	}
}

func TestSaveDraftRejects(t *testing.T) {
	e := newTestEngine(newFakeStore())
	ctx := context.Background()

	// Update by unknown ID → NotFound.
	if _, err := e.SaveDraft(ctx, SaveDraftInput{Match: ContentItemMatch{ID: "nope"}, Type: testType, Fields: validFields()}); err == nil {
		t.Fatal("expected NotFound for unknown id")
	} else {
		wantKind(t, err, KindNotFound)
	}

	// Create then attempt to change the type → BadRequest.
	item, _ := e.SaveDraft(ctx, SaveDraftInput{Type: testType, Fields: validFields()})
	if _, err := e.SaveDraft(ctx, SaveDraftInput{Match: ContentItemMatch{ID: item.ID}, Type: "other", Fields: validFields()}); err == nil {
		t.Fatal("expected BadRequest for type change")
	} else {
		wantKind(t, err, KindBadRequest)
	}
}
