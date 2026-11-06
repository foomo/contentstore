package contentstore

import (
	"context"
	"fmt"
	"slices"
	"testing"
)

func TestTagFilterMatches(t *testing.T) {
	tags := TagSet{"go": 1, "topic:db": 5}
	tests := []struct {
		filter *TagFilter
		want   bool
	}{
		{nil, true},
		{&TagFilter{}, true},
		{&TagFilter{All: []string{"go", "topic:db"}}, true},
		{&TagFilter{All: []string{"go", "rust"}}, false},
		{&TagFilter{Any: []string{"rust", "go"}}, true},
		{&TagFilter{Any: []string{"rust"}}, false},
		{&TagFilter{None: []string{"rust"}}, true},
		{&TagFilter{None: []string{"go"}}, false},
		{&TagFilter{All: []string{"go"}, Any: []string{"topic:db"}, None: []string{"rust"}}, true},
	}
	for _, tt := range tests {
		if got := tt.filter.matches(tags); got != tt.want {
			t.Errorf("%+v.matches = %v, want %v", tt.filter, got, tt.want)
		}
	}
}

// rankedArticles creates articles tagged topic:db with the given weights (published ones
// marked with a leading "+"), returning their IDs in creation order.
func rankedArticles(t *testing.T, e *Engine, weights ...string) []string {
	t.Helper()
	ctx := context.Background()
	var ids []string
	for _, w := range weights {
		publish := w[0] == '+'
		if publish {
			w = w[1:]
		}
		var weight float64
		if _, err := fmt.Sscan(w, &weight); err != nil {
			t.Fatalf("weight %q: %v", w, err)
		}
		item := newArticle(t, e)
		updateTags(t, e, UpdateTagsInput{Match: ContentItemMatch{ID: item.ID}, Set: map[string]*float64{"topic:db": &weight, "go": nil}})
		if publish {
			if _, err := e.Publish(ctx, ContentItemMatch{ID: item.ID}); err != nil {
				t.Fatalf("publish: %v", err)
			}
		}
		ids = append(ids, item.ID)
	}
	return ids
}

func pageIDs(page *ItemPage) []string {
	ids := make([]string, 0, len(page.Items))
	for _, item := range page.Items {
		ids = append(ids, item.ID)
	}
	return ids
}

// walkRanking pages through a ranking and returns every ID in order.
func walkRanking(t *testing.T, e *Engine, view SnapshotView, input ListTaggedInput) []string {
	t.Helper()
	var ids []string
	for range 100 {
		page, err := e.ListTagged(context.Background(), view, input)
		if err != nil {
			t.Fatalf("list tagged: %v", err)
		}
		ids = append(ids, pageIDs(page)...)
		if page.NextCursor == "" {
			return ids
		}
		input.Cursor = page.NextCursor
	}
	t.Fatal("ranking did not terminate")
	return nil
}

func TestListTaggedRanksByWeight(t *testing.T) {
	ctx := context.Background()
	e := newTagEngine(newFakeStore())
	// IDs id-1..id-6; ties between id-2/id-4 (5) and id-1/id-6 (1) order by descending ID.
	ids := rankedArticles(t, e, "+1", "+5", "+9", "5", "+0.5", "1")

	page, err := e.ListTagged(ctx, SnapshotViewPreview, ListTaggedInput{Tag: "topic:db"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	want := []string{ids[2], ids[3], ids[1], ids[5], ids[0], ids[4]}
	if got := pageIDs(page); !slices.Equal(got, want) || page.NextCursor != "" {
		t.Fatalf("preview ranking = %v (cursor %q), want %v", got, page.NextCursor, want)
	}
	if page.Items[0].Tags["topic:db"] != 9 {
		t.Fatalf("ranked items carry their tags: %+v", page.Items[0])
	}
	if got := walkRanking(t, e, SnapshotViewPreview, ListTaggedInput{Tag: "topic:db", Limit: 2}); !slices.Equal(got, want) {
		t.Fatalf("paged ranking = %v, want %v", got, want)
	}
	// The published view ranks published tags of published items only.
	wantPublished := []string{ids[2], ids[1], ids[0], ids[4]}
	if got := walkRanking(t, e, SnapshotViewPublished, ListTaggedInput{Tag: "topic:db", Limit: 3}); !slices.Equal(got, wantPublished) {
		t.Fatalf("published ranking = %v, want %v", got, wantPublished)
	}
	updateTags(t, e, UpdateTagsInput{Match: ContentItemMatch{ID: ids[0]}, Set: map[string]*float64{"topic:db": new(10.0)}})
	if got := walkRanking(t, e, SnapshotViewPublished, ListTaggedInput{Tag: "topic:db"}); got[0] != ids[2] {
		t.Fatalf("an unpublished re-weighting must not change the published ranking: %v", got)
	}
	if got := walkRanking(t, e, SnapshotViewPreview, ListTaggedInput{Tag: "topic:db"}); got[0] != ids[0] {
		t.Fatalf("the preview ranking follows the draft tags: %v", got)
	}

	// Archived items leave both rankings.
	if _, err := e.Archive(ctx, ContentItemMatch{ID: ids[2]}); err != nil {
		t.Fatalf("archive: %v", err)
	}
	for _, view := range []SnapshotView{SnapshotViewPreview, SnapshotViewPublished} {
		if got := walkRanking(t, e, view, ListTaggedInput{Tag: "topic:db"}); slices.Contains(got, ids[2]) {
			t.Fatalf("%s ranking contains an archived item: %v", view, got)
		}
	}
}

func TestListTaggedFiltersAndProjects(t *testing.T) {
	ctx := context.Background()
	e := newTagEngine(newFakeStore())
	ids := rankedArticles(t, e, "3", "2", "1")
	updateTags(t, e, UpdateTagsInput{Match: ContentItemMatch{ID: ids[1]}, Set: map[string]*float64{"audience:editors": nil}})

	got := walkRanking(t, e, SnapshotViewPreview, ListTaggedInput{Tag: "topic:db", Filter: &TagFilter{None: []string{"audience:editors"}}, Limit: 1})
	if want := []string{ids[0], ids[2]}; !slices.Equal(got, want) {
		t.Fatalf("filtered ranking = %v, want %v", got, want)
	}
	got = walkRanking(t, e, SnapshotViewPreview, ListTaggedInput{Tag: "topic:db", Filter: &TagFilter{All: []string{"audience:editors"}}})
	if want := []string{ids[1]}; !slices.Equal(got, want) {
		t.Fatalf("filtered ranking = %v, want %v", got, want)
	}
	if got := walkRanking(t, e, SnapshotViewPreview, ListTaggedInput{Tag: "topic:db", Types: []ContentType{documentType}}); len(got) != 0 {
		t.Fatalf("type filter = %v, want none", got)
	}
	page, err := e.ListTagged(ctx, SnapshotViewPreview, ListTaggedInput{Tag: "topic:db", Fields: []FieldID{"title"}, Limit: 1})
	if err != nil || len(page.Items) != 1 || page.Items[0].Fields["title"] == nil || len(page.Items[0].Tags) == 0 {
		t.Fatalf("projected ranking = %+v, %v", page, err)
	}
}

func TestListTaggedHidesItemsUnderUnpublishedFolders(t *testing.T) {
	ctx := context.Background()
	e := newTagEngine(newFakeStore())
	visibleFolder := place(t, e, folderType, "", "visible")
	hiddenFolder := place(t, e, folderType, "", "hidden")
	if _, err := e.Publish(ctx, ContentItemMatch{ID: visibleFolder.ID}); err != nil {
		t.Fatalf("publish folder: %v", err)
	}
	// Ten hidden documents rank above two visible ones.
	var hidden, shown []string
	for i := range 12 {
		parent, list := hiddenFolder, &hidden
		if i >= 10 {
			parent, list = visibleFolder, &shown
		}
		doc := place(t, e, documentType, parent.ID, fmt.Sprintf("doc-%d", i))
		updateTags(t, e, UpdateTagsInput{Match: ContentItemMatch{ID: doc.ID}, Set: map[string]*float64{"topic:db": new(float64(100 - i))}})
		if _, err := e.Publish(ctx, ContentItemMatch{ID: doc.ID}); err != nil {
			t.Fatalf("publish: %v", err)
		}
		*list = append(*list, doc.ID)
	}

	// Limit 2 allows 20 scanned rows: the hidden ten are skipped within one page.
	page, err := e.ListTagged(ctx, SnapshotViewPublished, ListTaggedInput{Tag: "topic:db", Limit: 2})
	if err != nil || !slices.Equal(pageIDs(page), shown) || page.NextCursor != "" {
		t.Fatalf("published ranking = %v (cursor %q), %v; want %v", pageIDs(page), page.NextCursor, err, shown)
	}
	// Limit 1 allows 10 scanned rows: the page comes back empty with a cursor, and the walk
	// still reaches every visible document exactly once.
	page, err = e.ListTagged(ctx, SnapshotViewPublished, ListTaggedInput{Tag: "topic:db", Limit: 1})
	if err != nil || len(page.Items) != 0 || page.NextCursor == "" {
		t.Fatalf("budgeted page = %v (cursor %q), %v; want an empty page with a cursor", pageIDs(page), page.NextCursor, err)
	}
	if got := walkRanking(t, e, SnapshotViewPublished, ListTaggedInput{Tag: "topic:db", Limit: 1}); !slices.Equal(got, shown) {
		t.Fatalf("budgeted walk = %v, want %v", got, shown)
	}
	if got := walkRanking(t, e, SnapshotViewPreview, ListTaggedInput{Tag: "topic:db"}); len(got) != 12 {
		t.Fatalf("preview ranks every document, got %d", len(got))
	}
}

func TestListTaggedRejects(t *testing.T) {
	ctx := context.Background()
	e := newTagEngine(newFakeStore())
	tests := map[string]struct {
		view  SnapshotView
		input ListTaggedInput
	}{
		"unknown view":   {"nope", ListTaggedInput{Tag: "go"}},
		"invalid tag":    {SnapshotViewPreview, ListTaggedInput{Tag: "topic:"}},
		"invalid filter": {SnapshotViewPreview, ListTaggedInput{Tag: "go", Filter: &TagFilter{Any: []string{" x"}}}},
		"limit":          {SnapshotViewPreview, ListTaggedInput{Tag: "go", Limit: 1001}},
		"cursor":         {SnapshotViewPreview, ListTaggedInput{Tag: "go", Cursor: "!"}},
		"unknown field":  {SnapshotViewPreview, ListTaggedInput{Tag: "go", Fields: []FieldID{"nope"}}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := e.ListTagged(ctx, tt.view, tt.input)
			wantKind(t, err, KindBadRequest)
		})
	}
	_, err := newTagEngine(storeOnly{newFakeStore()}).ListTagged(ctx, SnapshotViewPreview, ListTaggedInput{Tag: "go"})
	wantKind(t, err, KindInternal)
}

func TestTagFiltersOnSnapshotsAndChildren(t *testing.T) {
	ctx := context.Background()
	e := newTagEngine(newFakeStore())
	ids := rankedArticles(t, e, "+3", "+2", "1")
	updateTags(t, e, UpdateTagsInput{Match: ContentItemMatch{ID: ids[0]}, Remove: []string{"go"}})

	names := func(view SnapshotView, filter *TagFilter) []string {
		snapshot, err := e.GetSnapshot(ctx, view, SnapshotInput{Types: []ContentType{articleType}, Tags: filter})
		if err != nil {
			t.Fatalf("snapshot: %v", err)
		}
		var out []string
		for _, item := range snapshot.Items {
			out = append(out, item.ID)
		}
		slices.Sort(out)
		return out
	}
	if got, want := names(SnapshotViewPreview, &TagFilter{All: []string{"go"}}), []string{ids[1], ids[2]}; !slices.Equal(got, want) {
		t.Fatalf("preview filter = %v, want %v", got, want)
	}
	// ids[0] lost "go" only in its draft: the published view still matches it.
	if got, want := names(SnapshotViewPublished, &TagFilter{All: []string{"go"}}), []string{ids[0], ids[1]}; !slices.Equal(got, want) {
		t.Fatalf("published filter = %v, want %v", got, want)
	}
	_, err := e.GetSnapshot(ctx, SnapshotViewPreview, SnapshotInput{Tags: &TagFilter{All: []string{"topic:"}}})
	wantKind(t, err, KindBadRequest)

	kb := place(t, e, folderType, "", "kb")
	for i, tags := range []map[string]*float64{{"go": nil}, {"topic:db": new(2.0)}, {"go": nil, "topic:db": nil}} {
		doc := place(t, e, documentType, kb.ID, fmt.Sprintf("doc-%d", i))
		updateTags(t, e, UpdateTagsInput{Match: ContentItemMatch{ID: doc.ID}, Set: tags})
	}
	page, err := e.ListChildren(ctx, SnapshotViewPreview, ListChildrenInput{ParentID: kb.ID, Tags: &TagFilter{Any: []string{"topic:db"}, None: []string{"go"}}})
	if err != nil || !slices.Equal(names2(page), []string{"doc-1"}) {
		t.Fatalf("children filter = %v, %v", names2(page), err)
	}
}

func names2(page *ItemPage) []string {
	if page == nil {
		return nil
	}
	return names(page.Items)
}

func TestGetTagCounts(t *testing.T) {
	ctx := context.Background()
	e := newTagEngine(newFakeStore())
	ids := rankedArticles(t, e, "+3", "+2", "1")
	updateTags(t, e, UpdateTagsInput{Match: ContentItemMatch{ID: ids[2]}, Set: map[string]*float64{"topic:dart": nil}})
	if _, err := e.SaveDraft(ctx, SaveDraftInput{Type: testType, Fields: validFields()}); err != nil {
		t.Fatalf("untagged item: %v", err)
	}

	counts, err := e.GetTagCounts(ctx, SnapshotViewPreview, TagCountsInput{})
	if err != nil {
		t.Fatalf("counts: %v", err)
	}
	want := []TagCount{{"go", 3}, {"topic:db", 3}, {"topic:dart", 1}}
	if !slices.Equal(counts.Items, want) {
		t.Fatalf("preview counts = %v, want %v", counts.Items, want)
	}
	counts, err = e.GetTagCounts(ctx, SnapshotViewPreview, TagCountsInput{Prefix: "topic:da", Limit: 1})
	if err != nil || !slices.Equal(counts.Items, []TagCount{{"topic:dart", 1}}) {
		t.Fatalf("autocomplete = %v, %v", counts.Items, err)
	}
	counts, err = e.GetTagCounts(ctx, SnapshotViewPublished, TagCountsInput{Types: []ContentType{articleType}})
	if err != nil || !slices.Equal(counts.Items, []TagCount{{"go", 2}, {"topic:db", 2}}) {
		t.Fatalf("published counts = %v, %v", counts.Items, err)
	}
	_, err = e.GetTagCounts(ctx, SnapshotViewPreview, TagCountsInput{Limit: -1})
	wantKind(t, err, KindBadRequest)
}

// countingTagStore counts FindTagged round trips.
type countingTagStore struct {
	*fakeStore
	findTagged int
}

func (c *countingTagStore) FindTagged(ctx context.Context, q TagQuery) ([]*ContentItem, error) {
	c.findTagged++
	return c.fakeStore.FindTagged(ctx, q)
}

// TestListTaggedFetchesPageSizedBatches: a nearly full page followed by many hidden rows
// must not be refilled a row or two per round trip.
func TestListTaggedFetchesPageSizedBatches(t *testing.T) {
	store := &countingTagStore{fakeStore: newFakeStore()}
	e := newTagEngine(store)
	ids := rankedArticles(t, e, "10", "9.9", "9.8", "9.7")
	for i := range 30 { // hidden by the filter
		item := newArticle(t, e)
		updateTags(t, e, UpdateTagsInput{Match: ContentItemMatch{ID: item.ID}, Set: map[string]*float64{"topic:db": new(9 - float64(i)/10), "audience:editors": nil}})
	}
	ids = append(ids, rankedArticles(t, e, "1")...)

	store.findTagged = 0
	page, err := e.ListTagged(context.Background(), SnapshotViewPreview, ListTaggedInput{Tag: "topic:db", Limit: 5, Filter: &TagFilter{None: []string{"audience:editors"}}})
	if err != nil || !slices.Equal(pageIDs(page), ids) {
		t.Fatalf("page = %v, %v; want %v", pageIDs(page), err, ids)
	}
	if store.findTagged > 8 {
		t.Fatalf("one page took %d round trips", store.findTagged)
	}
}

// ignoringTagFilterStore is a TreeStore that ignores ChildQuery.Tags.
type ignoringTagFilterStore struct{ *fakeStore }

func (s ignoringTagFilterStore) FindChildren(ctx context.Context, q ChildQuery) ([]*ContentItem, error) {
	q.Tags = nil
	return s.fakeStore.FindChildren(ctx, q)
}

func TestListChildrenEnforcesTagFilter(t *testing.T) {
	ctx := context.Background()
	e := newTagEngine(ignoringTagFilterStore{newFakeStore()})
	kb := place(t, e, folderType, "", "kb")
	tagged := place(t, e, documentType, kb.ID, "tagged")
	place(t, e, documentType, kb.ID, "plain")
	updateTags(t, e, UpdateTagsInput{Match: ContentItemMatch{ID: tagged.ID}, Set: map[string]*float64{"go": nil}})
	page, err := e.ListChildren(ctx, SnapshotViewPreview, ListChildrenInput{ParentID: kb.ID, Tags: &TagFilter{All: []string{"go"}}})
	if err != nil || !slices.Equal(names2(page), []string{"tagged"}) {
		t.Fatalf("children = %v, %v", names2(page), err)
	}
}

// TestFilteredSnapshotRevisionSeesItemsLeavingTheFilter: the revision fingerprints the items
// before tag filtering, so untagging an item changes it even though the item drops out.
func TestFilteredSnapshotRevisionSeesItemsLeavingTheFilter(t *testing.T) {
	ctx := context.Background()
	e := newTagEngine(newFakeStore())
	ids := rankedArticles(t, e, "1", "2")
	input := SnapshotInput{Types: []ContentType{articleType}, Tags: &TagFilter{All: []string{"go"}}}
	before, err := e.GetSnapshot(ctx, SnapshotViewPreview, input)
	if err != nil || len(before.Items) != 2 {
		t.Fatalf("before: %+v, %v", before, err)
	}
	updateTags(t, e, UpdateTagsInput{Match: ContentItemMatch{ID: ids[0]}, Remove: []string{"go"}})
	after, err := e.GetSnapshot(ctx, SnapshotViewPreview, input)
	if err != nil || len(after.Items) != 1 || after.Revision == before.Revision {
		t.Fatalf("after: %d items, revision %d (before %d), %v", len(after.Items), after.Revision, before.Revision, err)
	}
}
