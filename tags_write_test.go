package contentstore

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
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

func mergeTagsOK(t *testing.T, e *Engine, input MergeTagsInput) *MergeTagsResult {
	t.Helper()
	res, err := e.MergeTags(context.Background(), input)
	if err != nil {
		t.Fatalf("merge tags %+v: %v", input, err)
	}
	return res
}

func TestMergeTags(t *testing.T) {
	ctx := context.Background()
	e := newTagEngine(newFakeStore())
	notifier := &recordingNotifier{}
	e.notify = notifier.record
	tagged := func(tags ...string) *ContentItem {
		t.Helper()
		set := map[string]*float64{}
		for _, tag := range tags {
			set[tag] = nil
		}
		return updateTags(t, e, UpdateTagsInput{Match: ContentItemMatch{ID: newArticle(t, e).ID}, Set: set})
	}
	publish := func(item *ContentItem) *ContentItem {
		t.Helper()
		published, err := e.Publish(ctx, ContentItemMatch{ID: item.ID})
		if err != nil {
			t.Fatalf("publish: %v", err)
		}
		return published
	}

	draftOnly := tagged("feedbck")
	published := publish(tagged("feedbck", "go"))
	pending := publish(tagged("feedbck"))
	if _, err := e.SaveDraft(ctx, SaveDraftInput{Match: ContentItemMatch{ID: pending.ID}, Fields: titleFields("v2")}); err != nil {
		t.Fatalf("save draft: %v", err)
	}
	both := tagged("feedbck", "feedback")
	publishedOnly := publish(tagged("feedbck"))
	updateTags(t, e, UpdateTagsInput{Match: ContentItemMatch{ID: publishedOnly.ID}, Remove: []string{"feedbck"}})
	archived := tagged("feedbck")
	if _, err := e.Archive(ctx, ContentItemMatch{ID: archived.ID}); err != nil {
		t.Fatalf("archive: %v", err)
	}
	other := tagged("go")
	before := map[string]*ContentItem{}
	for _, item := range []*ContentItem{draftOnly, published, pending, both, publishedOnly, archived, other} {
		before[item.ID] = getItem(t, e, item.ID)
	}
	notifier.changes = nil

	res := mergeTagsOK(t, e, MergeTagsInput{From: "feedbck", To: "feedback"})
	if res.Merged != 5 || len(res.Failed) != 0 {
		t.Fatalf("result = %+v, want 5 merged", res)
	}
	check := func(name string, item *ContentItem, draftTags, publishedTags TagSet, state string) *ContentItem {
		t.Helper()
		got := getItem(t, e, item.ID)
		if !maps.Equal(got.Draft.Tags, draftTags) {
			t.Errorf("%s: draft tags = %v, want %v", name, got.Draft.Tags, draftTags)
		}
		if got.Published != nil && !maps.Equal(got.Published.Tags, publishedTags) {
			t.Errorf("%s: published tags = %v, want %v", name, got.Published.Tags, publishedTags)
		}
		if got.State() != state {
			t.Errorf("%s: state = %s, want %s", name, got.State(), state)
		}
		if got.Draft.Revision != before[item.ID].Draft.Revision+1 {
			t.Errorf("%s: draft revision = %d, want %d", name, got.Draft.Revision, before[item.ID].Draft.Revision+1)
		}
		return got
	}
	check("draft only", draftOnly, TagSet{"feedback": 1}, nil, StateDraft)
	got := check("published", published, TagSet{"feedback": 1, "go": 1}, TagSet{"feedback": 1, "go": 1}, StatePublished)
	if got.Published.Fields["title"]["de"] != "v1" {
		t.Errorf("published fields changed: %v", got.Published.Fields)
	}
	got = check("pending edits", pending, TagSet{"feedback": 1}, TagSet{"feedback": 1}, StateChanged)
	if got.Draft.Fields["title"]["de"] != "v2" || got.Published.Fields["title"]["de"] != "v1" {
		t.Errorf("pending edits must stay pending: draft %v, published %v", got.Draft.Fields, got.Published.Fields)
	}
	check("both tags", both, TagSet{"feedback": 1}, nil, StateDraft)
	check("published only", publishedOnly, nil, TagSet{"feedback": 1}, StateChanged)
	for _, item := range []*ContentItem{archived, other} {
		if got := getItem(t, e, item.ID); got.Draft.Revision != before[item.ID].Draft.Revision || !maps.Equal(got.Draft.Tags, before[item.ID].Draft.Tags) {
			t.Errorf("item %s must be untouched: %+v", item.ID, got.Draft)
		}
	}

	ops := map[string]string{}
	for _, c := range notifier.changes {
		ops[c.ID] = c.Op
	}
	wantOps := map[string]string{
		draftOnly.ID: OpSaveDraft, both.ID: OpSaveDraft,
		published.ID: OpMergeTags, pending.ID: OpMergeTags, publishedOnly.ID: OpMergeTags,
	}
	if len(notifier.changes) != len(wantOps) || !maps.Equal(ops, wantOps) {
		t.Errorf("events = %+v, want ops %v", notifier.changes, wantOps)
	}

	for view, want := range map[SnapshotView][]TagCount{
		SnapshotViewPreview:   {{ID: "feedback", Count: 4}, {ID: "go", Count: 2}},
		SnapshotViewPublished: {{ID: "feedback", Count: 3}, {ID: "go", Count: 1}},
	} {
		counts, err := e.GetTagCounts(ctx, view, TagCountsInput{})
		if err != nil || !slices.Equal(counts.Items, want) {
			t.Errorf("%s tag counts = %+v, %v; want %+v", view, counts, err, want)
		}
	}

	// Repeating the merge finds nothing left to do.
	notifier.changes = nil
	if res := mergeTagsOK(t, e, MergeTagsInput{From: "feedbck", To: "feedback"}); res.Merged != 0 || len(res.Failed) != 0 || len(notifier.changes) != 0 {
		t.Fatalf("repeated merge = %+v, events %+v", res, notifier.changes)
	}
}

func TestMergeTagsReportsRejectedItems(t *testing.T) {
	e := newTagEngine(newFakeStore())
	article := updateTags(t, e, UpdateTagsInput{Match: ContentItemMatch{ID: newArticle(t, e).ID}, Set: map[string]*float64{"devs": nil}})
	doc := place(t, e, documentType, "", "doc")
	updateTags(t, e, UpdateTagsInput{Match: ContentItemMatch{ID: doc.ID}, Set: map[string]*float64{"devs": nil}})

	// Types narrows the merge: documents, whose policy has no audience namespace, are left out.
	input := MergeTagsInput{From: "devs", To: "audience:developers", Types: []ContentType{articleType}}
	if res := mergeTagsOK(t, e, input); res.Merged != 1 || len(res.Failed) != 0 {
		t.Fatalf("article merge = %+v", res)
	}
	if got := getItem(t, e, article.ID); !maps.Equal(got.Draft.Tags, TagSet{"audience:developers": 1}) {
		t.Fatalf("article tags = %v", got.Draft.Tags)
	}

	input.Types = nil
	res := mergeTagsOK(t, e, input)
	want := []MergeTagsFailure{{ID: doc.ID, Error: `tag "audience:developers": namespace "audience" is not allowed`}}
	if res.Merged != 0 || !slices.Equal(res.Failed, want) {
		t.Fatalf("result = %+v, want failures %+v", res, want)
	}
	if got := getItem(t, e, doc.ID); !maps.Equal(got.Draft.Tags, TagSet{"devs": 1}) {
		t.Fatalf("a rejected item must keep its tags: %v", got.Draft.Tags)
	}
}

func TestMergeTagsRejects(t *testing.T) {
	e := newTagEngine(newFakeStore())
	for name, input := range map[string]MergeTagsInput{
		"no from":       {To: "go"},
		"no to":         {From: "go"},
		"same tag":      {From: "go", To: "go"},
		"invalid to":    {From: "go", To: "topic:"},
		"bad namespace": {From: "go", To: "Topic:go"},
	} {
		_, err := e.MergeTags(context.Background(), input)
		var failure *Error
		if !errors.As(err, &failure) || failure.Kind != KindBadRequest {
			t.Errorf("%s: err = %v, want a bad request", name, err)
		}
	}
}

func TestMergeTagInto(t *testing.T) {
	policy := &TagPolicy{Namespaces: []TagNamespace{
		{Name: ""},
		{Name: "topic", Weighted: true, MinWeight: new(0.0), MaxWeight: new(10.0)},
		{Name: "rank", Weighted: true},
		{Name: "audience", Values: []string{"developers"}},
	}}
	tests := []struct {
		name     string
		noPolicy bool
		tags     TagSet
		from, to string
		want     TagSet
		wantErr  bool
	}{
		{name: "rename", tags: TagSet{"feedbck": 1, "go": 1}, from: "feedbck", to: "feedback", want: TagSet{"feedback": 1, "go": 1}},
		{name: "keeps the weight", tags: TagSet{"topic:databse": 3}, from: "topic:databse", to: "topic:database", want: TagSet{"topic:database": 3}},
		{name: "higher source weight wins", tags: TagSet{"topic:databse": 8, "topic:database": 2}, from: "topic:databse", to: "topic:database", want: TagSet{"topic:database": 8}},
		{name: "higher target weight wins", tags: TagSet{"topic:databse": 3, "topic:database": 7}, from: "topic:databse", to: "topic:database", want: TagSet{"topic:database": 7}},
		{name: "unweighted target weighs 1", tags: TagSet{"topic:mongodb": 4}, from: "topic:mongodb", to: "mongodb", want: TagSet{"mongodb": 1}},
		{name: "into another namespace", tags: TagSet{"mongodb": 1}, from: "mongodb", to: "topic:mongodb", want: TagSet{"topic:mongodb": 1}},
		{name: "malformed source", tags: TagSet{" go": 1}, from: " go", to: "go", want: TagSet{"go": 1}},
		{name: "absent source", noPolicy: true, tags: TagSet{"go": 1}, from: "feedbck", to: "feedback", want: nil},
		{name: "weight out of range", tags: TagSet{"rank:go": 12}, from: "rank:go", to: "topic:go", wantErr: true},
		{name: "closed namespace", tags: TagSet{"devs": 1}, from: "devs", to: "audience:devs", wantErr: true},
		{name: "unknown namespace", tags: TagSet{"red": 1}, from: "red", to: "color:red", wantErr: true},
		{name: "no policy", noPolicy: true, tags: TagSet{"go": 1}, from: "go", to: "golang", wantErr: true},
	}
	for _, tt := range tests {
		p := policy
		if tt.noPolicy {
			p = nil
		}
		original := maps.Clone(tt.tags)
		got, err := mergeTagInto(articleType, p, tt.tags, tt.from, tt.to)
		if (err != nil) != tt.wantErr || !maps.Equal(got, tt.want) || (got == nil) != (tt.want == nil) {
			t.Errorf("%s: got %v, %v; want %v (error %v)", tt.name, got, err, tt.want, tt.wantErr)
		}
		if !maps.Equal(tt.tags, original) {
			t.Errorf("%s: input mutated to %v", tt.name, tt.tags)
		}
	}
}

// TestMergeTagsRetriesConcurrentPublish publishes between a merge's load and its write.
// Publishing does not bump the draft revision, so only the published precondition stops the
// merge from leaving the freshly published snapshot with the old tag.
func TestMergeTagsRetriesConcurrentPublish(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	e := newTagEngine(store)
	item := newArticle(t, e)
	match := ContentItemMatch{ID: item.ID}
	if _, err := e.Publish(ctx, match); err != nil {
		t.Fatalf("publish: %v", err)
	}
	updateTags(t, e, UpdateTagsInput{Match: match, Set: map[string]*float64{"feedbck": nil}})
	store.beforeWrite = func() {
		store.beforeWrite = nil
		if _, err := e.Publish(ctx, match); err != nil {
			t.Errorf("concurrent publish: %v", err)
		}
	}
	if res := mergeTagsOK(t, e, MergeTagsInput{From: "feedbck", To: "feedback"}); res.Merged != 1 {
		t.Fatalf("result = %+v", res)
	}
	got := getItem(t, e, item.ID)
	want := TagSet{"feedback": 1}
	if !maps.Equal(got.Draft.Tags, want) || !maps.Equal(got.Published.Tags, want) || got.State() != StatePublished {
		t.Fatalf("draft %v, published %v, state %s; want both %v and published", got.Draft.Tags, got.Published.Tags, got.State(), want)
	}
}

func TestMergeTagsPagesThroughCarriers(t *testing.T) {
	store := newFakeStore()
	e := newTagEngine(store)
	n := 2*mergeTagsBatchSize + 5
	for i := range n {
		id := fmt.Sprintf("item-%04d", i)
		store.items[id] = &ContentItem{ID: id, Type: articleType, Draft: &ContentSnapshot{Revision: 1, Tags: TagSet{"topic:databse": float64(i % 7)}}}
	}
	if res := mergeTagsOK(t, e, MergeTagsInput{From: "topic:databse", To: "topic:database"}); res.Merged != n || len(res.Failed) != 0 {
		t.Fatalf("merged %d of %d, failed %+v", res.Merged, n, res.Failed)
	}
	for id, item := range store.items {
		if _, ok := item.Draft.Tags["topic:database"]; !ok || len(item.Draft.Tags) != 1 {
			t.Fatalf("%s tags = %v", id, item.Draft.Tags)
		}
	}
}
