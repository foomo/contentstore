package contentstore

import (
	"context"
	"maps"
	"testing"
)

func newArticle(t *testing.T, e *Engine) *ContentItem {
	t.Helper()
	item, err := e.SaveDraft(context.Background(), SaveDraftInput{Type: articleType, Fields: titleFields("v1")})
	if err != nil {
		t.Fatalf("create article: %v", err)
	}
	return item
}

func updateTags(t *testing.T, e *Engine, input UpdateTagsInput) *ContentItem {
	t.Helper()
	item, err := e.UpdateTags(context.Background(), input)
	if err != nil {
		t.Fatalf("update tags %+v: %v", input, err)
	}
	return item
}

func TestUpdateTags(t *testing.T) {
	ctx := context.Background()
	e := newTagEngine(newFakeStore())
	notifier := &recordingNotifier{}
	e.notify = notifier.record
	item := newArticle(t, e)
	match := ContentItemMatch{ID: item.ID}
	if _, err := e.Publish(ctx, match); err != nil {
		t.Fatalf("publish: %v", err)
	}
	notifier.changes = nil

	got := updateTags(t, e, UpdateTagsInput{Match: match, Set: map[string]*float64{
		"mongodb": nil, "topic:databases": new(7.5), "audience:developers": nil,
	}})
	if want := (TagSet{"mongodb": 1, "topic:databases": 7.5, "audience:developers": 1}); !maps.Equal(got.Draft.Tags, want) {
		t.Fatalf("tags = %v, want %v", got.Draft.Tags, want)
	}
	if got.Draft.Revision != 2 || got.State() != StateChanged || got.Published.Tags != nil {
		t.Fatalf("tags are draft content: revision=%d state=%s published=%v", got.Draft.Revision, got.State(), got.Published.Tags)
	}

	got = updateTags(t, e, UpdateTagsInput{Match: match, Set: map[string]*float64{"topic:databases": new(9.0)}, Remove: []string{"mongodb"}})
	if want := (TagSet{"topic:databases": 9, "audience:developers": 1}); !maps.Equal(got.Draft.Tags, want) {
		t.Fatalf("merged tags = %v, want %v", got.Draft.Tags, want)
	}
	got = updateTags(t, e, UpdateTagsInput{Match: match, Replace: true, Set: map[string]*float64{"go": nil}})
	if want := (TagSet{"go": 1}); !maps.Equal(got.Draft.Tags, want) {
		t.Fatalf("replaced tags = %v, want %v", got.Draft.Tags, want)
	}

	// A write that changes nothing is a no-op.
	before := got.Draft.Revision
	got = updateTags(t, e, UpdateTagsInput{Match: match, Set: map[string]*float64{"go": nil}, Remove: []string{"absent"}})
	if got.Draft.Revision != before {
		t.Fatalf("no-op bumped the revision to %d", got.Draft.Revision)
	}

	got = updateTags(t, e, UpdateTagsInput{Match: match, Replace: true})
	if len(got.Draft.Tags) != 0 {
		t.Fatalf("an empty replace must clear the tags, got %v", got.Draft.Tags)
	}
	if len(notifier.changes) != 4 {
		t.Fatalf("expected 4 events (no-op emits nothing), got %+v", notifier.changes)
	}
	for _, c := range notifier.changes {
		if c.Op != OpSaveDraft || c.ID != item.ID {
			t.Fatalf("unexpected change %+v", c)
		}
	}

	// Published after tagging: tags are published with the content.
	updateTags(t, e, UpdateTagsInput{Match: match, Set: map[string]*float64{"go": nil}})
	published, err := e.Publish(ctx, match)
	if err != nil || published.Published.Tags["go"] != 1 {
		t.Fatalf("publish must carry the tags: %+v, %v", published.Published, err)
	}
}

func TestUpdateTagsRejects(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	e := newTagEngine(store)
	item := newArticle(t, e)
	match := ContentItemMatch{ID: item.ID}
	legacy, err := e.SaveDraft(ctx, SaveDraftInput{Type: testType, Fields: validFields()})
	if err != nil {
		t.Fatalf("legacy: %v", err)
	}
	archived := newArticle(t, e)
	if _, err := e.Archive(ctx, ContentItemMatch{ID: archived.ID}); err != nil {
		t.Fatalf("archive: %v", err)
	}
	full := newArticle(t, e)
	seedTags(t, store, full.ID, TagSet{"a": 1, "b": 1, "c": 1, "d": 1, "e": 1})

	tests := []struct {
		name  string
		input UpdateTagsInput
		kind  Kind
	}{
		{"empty match", UpdateTagsInput{Set: map[string]*float64{"go": nil}}, KindBadRequest},
		{"replace with remove", UpdateTagsInput{Match: match, Replace: true, Remove: []string{"go"}}, KindBadRequest},
		{"set and remove the same tag", UpdateTagsInput{Match: match, Set: map[string]*float64{"go": nil}, Remove: []string{"go"}}, KindBadRequest},
		{"empty removed tag", UpdateTagsInput{Match: match, Remove: []string{""}}, KindBadRequest},
		{"type without tags", UpdateTagsInput{Match: ContentItemMatch{ID: legacy.ID}, Set: map[string]*float64{"go": nil}}, KindBadRequest},
		{"closed namespace", UpdateTagsInput{Match: match, Set: map[string]*float64{"audience:managers": nil}}, KindBadRequest},
		{"weight on an unweighted namespace", UpdateTagsInput{Match: match, Set: map[string]*float64{"go": new(2.0)}}, KindBadRequest},
		{"weight out of range", UpdateTagsInput{Match: match, Set: map[string]*float64{"topic:x": new(11.0)}}, KindBadRequest},
		{"too many tags", UpdateTagsInput{Match: ContentItemMatch{ID: full.ID}, Set: map[string]*float64{"f": nil}}, KindBadRequest},
		{"missing item", UpdateTagsInput{Match: ContentItemMatch{ID: "nope"}, Set: map[string]*float64{"go": nil}}, KindNotFound},
		{"archived item", UpdateTagsInput{Match: ContentItemMatch{ID: archived.ID}, Set: map[string]*float64{"go": nil}}, KindNotAcceptable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := e.UpdateTags(ctx, tt.input)
			wantKind(t, err, tt.kind)
		})
	}

	// An item over the limit (e.g. after MaxTags was lowered) can still shed and re-weigh tags.
	seedTags(t, store, full.ID, TagSet{"a": 1, "b": 1, "c": 1, "d": 1, "e": 1, "f": 1, "topic:x": 1})
	updateTags(t, e, UpdateTagsInput{Match: ContentItemMatch{ID: full.ID}, Set: map[string]*float64{"topic:x": new(3.0)}, Remove: []string{"a"}})

	// A malformed stored tag (e.g. from an old import) can still be removed by its key.
	seedTags(t, store, item.ID, TagSet{"topic: x ": 1, "go": 1})
	got := updateTags(t, e, UpdateTagsInput{Match: match, Remove: []string{"topic: x "}})
	if want := (TagSet{"go": 1}); !maps.Equal(got.Draft.Tags, want) {
		t.Fatalf("after removing the malformed tag: %v, want %v", got.Draft.Tags, want)
	}

	_, err = newTagEngine(storeOnly{newFakeStore()}).UpdateTags(ctx, UpdateTagsInput{Match: match})
	wantKind(t, err, KindInternal)
}

// TestTaggedWritesNeverLoseConcurrentChanges injects a concurrent tag write between each
// operation's load and its write: the operation must retry instead of overwriting it, and
// every revision number must be used once.
func TestTaggedWritesNeverLoseConcurrentChanges(t *testing.T) {
	ctx := context.Background()
	ops := map[string]func(e *Engine, match ContentItemMatch) error{
		"UpdateTags": func(e *Engine, match ContentItemMatch) error {
			_, err := e.UpdateTags(ctx, UpdateTagsInput{Match: match, Set: map[string]*float64{"mine": nil}})
			return err
		},
		"SaveDraft": func(e *Engine, match ContentItemMatch) error {
			_, err := e.SaveDraft(ctx, SaveDraftInput{Match: match, Fields: titleFields("v2")})
			return err
		},
		"Publish": func(e *Engine, match ContentItemMatch) error {
			_, err := e.Publish(ctx, match)
			return err
		},
	}
	for name, op := range ops {
		t.Run(name, func(t *testing.T) {
			store := newFakeStore()
			e := newTagEngine(store)
			item := newArticle(t, e)
			match := ContentItemMatch{ID: item.ID}
			store.beforeWrite = func() {
				store.beforeWrite = nil
				if _, err := e.UpdateTags(ctx, UpdateTagsInput{Match: match, Set: map[string]*float64{"concurrent": nil}}); err != nil {
					t.Errorf("concurrent write: %v", err)
				}
			}
			if err := op(e, match); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			got := getItem(t, e, item.ID)
			if got.Draft.Tags["concurrent"] != 1 {
				t.Fatalf("%s overwrote the concurrent tag write: %v", name, got.Draft.Tags)
			}
			wantRevision := int64(2) // create + concurrent write
			if name != "Publish" {
				wantRevision = 3 // ... + the operation's own draft write
			}
			if got.Draft.Revision != wantRevision {
				t.Fatalf("draft revision = %d, want %d", got.Draft.Revision, wantRevision)
			}
			if name == "Publish" && (got.Published == nil || got.Published.Tags["concurrent"] != 1 || got.State() != StatePublished) {
				t.Fatalf("publish must retry on the latest draft: %+v", got.Published)
			}
		})
	}

	t.Run("gives up after repeated conflicts", func(t *testing.T) {
		store := newFakeStore()
		e := newTagEngine(store)
		item := newArticle(t, e)
		match := ContentItemMatch{ID: item.ID}
		var interfere func()
		interfere = func() {
			store.beforeWrite = nil
			if _, err := e.UpdateTags(ctx, UpdateTagsInput{Match: match, Set: map[string]*float64{"topic:x": new(float64(store.items[item.ID].Draft.Revision))}}); err != nil {
				t.Errorf("interfering write: %v", err)
			}
			store.beforeWrite = interfere
		}
		store.beforeWrite = interfere
		_, err := e.Publish(ctx, match)
		store.beforeWrite = nil
		wantKind(t, err, KindConflict)
	})
}
