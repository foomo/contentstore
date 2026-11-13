package contentstore

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// TestTagSetRoundTripsInMongo stores tags through the persistor (whose registry decodes
// embedded documents as bson.M) and checks they come back intact and are stored escaped.
func TestTagSetRoundTripsInMongo(t *testing.T) {
	ctx := context.Background()
	store, err := NewMongoStore(ctx, mongoURLForTest(t))
	if err != nil {
		t.Fatalf("NewMongoStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	tags := TagSet{"topic:node.js": 2.5, "2024": 1, "$money": 0.5, "100%": 3}
	item := &ContentItem{ID: "tagged", Type: articleType, Draft: &ContentSnapshot{Revision: 1, Tags: tags}}
	if err := store.Upsert(ctx, item, nil); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, err := store.GetByID(ctx, "tagged")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !maps.Equal(got.Draft.Tags, tags) {
		t.Fatalf("tags = %v, want %v", got.Draft.Tags, tags)
	}

	var raw bson.M
	if err := store.coll.FindOne(ctx, bson.D{{Key: "_id", Value: "tagged"}}).Decode(&raw); err != nil {
		t.Fatalf("raw: %v", err)
	}
	stored := raw["draft"].(bson.M)["tags"].(bson.M)
	for _, key := range []string{"topic:node%2Ejs", "%32024", "%24money", "100%25"} {
		if _, ok := stored[key]; !ok {
			t.Fatalf("stored tag keys = %v, missing %q", stored, key)
		}
	}
	// An escaped key is addressable by a dotted path.
	n, err := store.coll.CountDocuments(ctx, bson.D{{Key: "draft.tags.topic:node%2Ejs", Value: 2.5}})
	if err != nil || n != 1 {
		t.Fatalf("query by escaped path matched %d, %v", n, err)
	}
}

func TestPersistorUpdateTagsIsCompareAndSwap(t *testing.T) {
	ctx := context.Background()
	e, store := newMongoTreeEngine(t)
	item := newArticle(t, e)

	_, err := store.UpdateTags(ctx, TagUpdate{ID: item.ID, ExpectedRevision: 7, Tags: TagSet{"go": 1}})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("stale revision: err = %v, want ErrConflict", err)
	}
	got, err := store.UpdateTags(ctx, TagUpdate{ID: item.ID, ExpectedRevision: 1, Tags: TagSet{"topic:node.js": 2}})
	if err != nil || got.Draft.Revision != 2 || got.Draft.Tags["topic:node.js"] != 2 || got.Draft.Fields["title"]["de"] != "v1" {
		t.Fatalf("update = %+v, %v", got, err)
	}
	got, err = store.UpdateTags(ctx, TagUpdate{ID: item.ID, ExpectedRevision: 2})
	if err != nil || got.Draft.Tags != nil {
		t.Fatalf("clearing: %+v, %v", got, err)
	}
	var raw bson.M
	if err := store.coll.FindOne(ctx, bson.D{{Key: "_id", Value: item.ID}}).Decode(&raw); err != nil {
		t.Fatalf("raw: %v", err)
	}
	if _, ok := raw["draft"].(bson.M)["tags"]; ok {
		t.Fatalf("an empty tag set must be unset: %v", raw["draft"])
	}
	if _, err := e.Archive(ctx, ContentItemMatch{ID: item.ID}); err != nil {
		t.Fatalf("archive: %v", err)
	}
	if _, err := store.UpdateTags(ctx, TagUpdate{ID: item.ID, ExpectedRevision: 3, Tags: TagSet{"go": 1}}); !errors.Is(err, ErrConflict) {
		t.Fatalf("archived item: err = %v, want ErrConflict", err)
	}
}

func TestConcurrentTagWritesAllLandInMongo(t *testing.T) {
	ctx := context.Background()
	e, _ := newMongoTreeEngine(t)
	item := newArticle(t, e)
	match := ContentItemMatch{ID: item.ID}

	writers := make([]func() error, 0, 5)
	for _, tag := range []string{"a", "b", "c", "d", "e"} {
		writers = append(writers, func() error {
			_, err := e.UpdateTags(ctx, UpdateTagsInput{Match: match, Set: map[string]*float64{tag: nil}})
			return err
		})
	}
	if err := errors.Join(runConcurrently(writers...)...); err != nil {
		t.Fatalf("concurrent tag writes: %v", err)
	}
	got := getItem(t, e, item.ID)
	if len(got.Draft.Tags) != 5 || got.Draft.Revision != 6 {
		t.Fatalf("tags = %v revision = %d, want 5 tags and revision 6", got.Draft.Tags, got.Draft.Revision)
	}
}

func TestTagWritesRaceDraftWritesInMongo(t *testing.T) {
	ctx := context.Background()
	e, _ := newMongoTreeEngine(t)
	for round := range 10 {
		item := newArticle(t, e)
		match := ContentItemMatch{ID: item.ID}
		tag := fmt.Sprintf("round-%d", round)
		errs := runConcurrently(
			func() error {
				_, err := e.UpdateTags(ctx, UpdateTagsInput{Match: match, Set: map[string]*float64{tag: nil}})
				return err
			},
			func() error {
				_, err := e.SaveDraft(ctx, SaveDraftInput{Match: match, Fields: titleFields("v2")})
				return err
			},
			func() error { _, err := e.Publish(ctx, match); return err },
		)
		if err := errors.Join(errs...); err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
		got := getItem(t, e, item.ID)
		if got.Draft.Tags[tag] != 1 || got.Draft.Fields["title"]["de"] != "v2" || got.Draft.Revision != 3 {
			t.Fatalf("round %d: lost a write: tags=%v title=%v revision=%d", round, got.Draft.Tags, got.Draft.Fields["title"], got.Draft.Revision)
		}
	}
}

// planStages returns the stages of the winning plan of an explained find command.
func planStages(t *testing.T, store *Persistor, find bson.D) []string {
	t.Helper()
	var res bson.M
	cmd := bson.D{{Key: "explain", Value: find}, {Key: "verbosity", Value: "queryPlanner"}}
	if err := store.coll.Database().RunCommand(context.Background(), cmd).Decode(&res); err != nil {
		t.Fatalf("explain: %v", err)
	}
	plan := res["queryPlanner"].(bson.M)["winningPlan"].(bson.M)
	if inner, ok := plan["queryPlan"].(bson.M); ok {
		plan = inner
	}
	var stages []string
	var walk func(bson.M)
	walk = func(n bson.M) {
		stage := fmt.Sprint(n["stage"])
		if index, ok := n["indexName"]; ok {
			stage += "(" + fmt.Sprint(index) + ")"
		}
		stages = append(stages, stage)
		if in, ok := n["inputStage"].(bson.M); ok {
			walk(in)
		}
		if ins, ok := n["inputStages"].(bson.A); ok {
			for _, in := range ins {
				walk(in.(bson.M))
			}
		}
	}
	walk(plan)
	return stages
}

// tagRankedArticles creates published articles tagged topic:a (weight i) and 2024 (weight
// i%3, many ties), returning their IDs.
func tagRankedArticles(t *testing.T, e *Engine, n int) []string {
	t.Helper()
	ctx := context.Background()
	ids := make([]string, 0, n)
	for i := range n {
		item := newArticle(t, e)
		updateTags(t, e, UpdateTagsInput{Match: ContentItemMatch{ID: item.ID}, Set: map[string]*float64{
			"topic:a": new(float64(i % 10)), "2024": nil, "topic:node.js": new(float64(i % 3)),
		}})
		if _, err := e.Publish(ctx, ContentItemMatch{ID: item.ID}); err != nil {
			t.Fatalf("publish: %v", err)
		}
		ids = append(ids, item.ID)
	}
	return ids
}

// TestTagRankingIsIndexBackedInMongo asserts that the ranking query FindTagged sends never
// needs a blocking sort with tag indexes enabled: first and keyset pages, both views,
// with and without a type condition, for namespaced, dotted and all-digit tags.
func TestTagRankingIsIndexBackedInMongo(t *testing.T) {
	e, store := newMongoTreeEngine(t, WithTagIndexes())
	tagRankedArticles(t, e, 20)
	for _, view := range []SnapshotView{SnapshotViewPublished, SnapshotViewPreview} {
		for _, tag := range []string{"topic:a", "2024", "topic:node.js"} {
			for _, after := range []*TagCursor{nil, {Weight: 1, ID: "id-5"}} {
				for _, types := range [][]ContentType{nil, {articleType}} {
					q := TagQuery{Tag: tag, View: view, Types: types, After: after, Fields: []FieldID{"title"}, Limit: 10}
					filter, sort, projection := tagQueryParts(q)
					find := bson.D{
						{Key: "find", Value: collectionContentItems}, {Key: "filter", Value: filter}, {Key: "sort", Value: sort},
						{Key: "projection", Value: projection}, {Key: "limit", Value: 10}, {Key: "hint", Value: tagRankIndex(view)},
					}
					stages := planStages(t, store, find)
					if slices.Contains(stages, "SORT") || !slices.ContainsFunc(stages, func(s string) bool { return strings.HasPrefix(s, "IXSCAN("+tagRankIndex(view)) }) {
						t.Errorf("view=%s tag=%s after=%v types=%v: plan %v needs an in-memory sort", view, tag, after, types, stages)
					}
				}
			}
		}
	}
}

func TestListTaggedInMongo(t *testing.T) {
	ctx := context.Background()
	e, _ := newMongoTreeEngine(t, WithTagIndexes())
	ids := tagRankedArticles(t, e, 12)

	for _, tag := range []string{"topic:node.js", "2024"} {
		got := walkRanking(t, e, SnapshotViewPublished, ListTaggedInput{Tag: tag, Limit: 5})
		if len(got) != len(ids) {
			t.Fatalf("%s: ranking has %d items, want %d", tag, len(got), len(ids))
		}
		page, err := e.ListTagged(ctx, SnapshotViewPublished, ListTaggedInput{Tag: tag, Limit: 12})
		if err != nil {
			t.Fatalf("%s: %v", tag, err)
		}
		for i := 1; i < len(page.Items); i++ {
			prev, cur := page.Items[i-1], page.Items[i]
			if pw, cw := prev.Tags[tag], cur.Tags[tag]; pw < cw || (pw == cw && prev.ID < cur.ID) {
				t.Fatalf("%s: %s (%v) ranks before %s (%v)", tag, prev.ID, pw, cur.ID, cw)
			}
		}
	}

	kb := place(t, e, folderType, "", "kb")
	doc := place(t, e, documentType, kb.ID, "doc")
	updateTags(t, e, UpdateTagsInput{Match: ContentItemMatch{ID: doc.ID}, Set: map[string]*float64{"topic:a": new(1000.0)}})
	if _, err := e.Publish(ctx, ContentItemMatch{ID: doc.ID}); err != nil {
		t.Fatalf("publish doc: %v", err)
	}
	page, err := e.ListTagged(ctx, SnapshotViewPublished, ListTaggedInput{Tag: "topic:a", Limit: 1})
	if err != nil || len(page.Items) != 1 || page.Items[0].ID == doc.ID {
		t.Fatalf("a document under an unpublished folder must not rank: %+v, %v", page, err)
	}
	children, err := e.ListChildren(ctx, SnapshotViewPreview, ListChildrenInput{ParentID: kb.ID, Tags: &TagFilter{All: []string{"topic:a"}}})
	if err != nil || len(children.Items) != 1 {
		t.Fatalf("children tag filter: %+v, %v", children, err)
	}
	children, err = e.ListChildren(ctx, SnapshotViewPreview, ListChildrenInput{ParentID: kb.ID, Tags: &TagFilter{None: []string{"topic:a"}}})
	if err != nil || len(children.Items) != 0 {
		t.Fatalf("children tag filter (none): %+v, %v", children, err)
	}
}

func TestTagIndexesSurviveReplaceAllInMongo(t *testing.T) {
	ctx := context.Background()
	e, store := newMongoTreeEngine(t, WithTagIndexes())
	tagRankedArticles(t, e, 3)
	items, err := store.ExportAll(ctx)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if err := store.ReplaceAll(ctx, items); err != nil {
		t.Fatalf("replace: %v", err)
	}
	specs, err := store.coll.Indexes().ListSpecifications(ctx)
	if err != nil {
		t.Fatalf("list indexes: %v", err)
	}
	var names []string
	for _, spec := range specs {
		names = append(names, spec.Name)
	}
	for _, want := range []string{indexDraftTagsRank, indexPublishedTagsRank} {
		if !slices.Contains(names, want) {
			t.Fatalf("indexes after ReplaceAll = %v, missing %s", names, want)
		}
	}

	// Without the ranking index the hinted query falls back to an unhinted one.
	if err := store.coll.Indexes().DropOne(ctx, indexPublishedTagsRank); err != nil {
		t.Fatalf("drop index: %v", err)
	}
	got := walkRanking(t, e, SnapshotViewPublished, ListTaggedInput{Tag: "topic:a"})
	if len(got) != 3 || !store.tagHintMissing.Load() {
		t.Fatalf("ranking without its index = %v (hint dropped: %v)", got, store.tagHintMissing.Load())
	}
}

func TestPersistorMergeTagsIsCompareAndSwap(t *testing.T) {
	ctx := context.Background()
	e, store := newMongoTreeEngine(t)
	item := newArticle(t, e)
	match := ContentItemMatch{ID: item.ID}
	updateTags(t, e, UpdateTagsInput{Match: match, Set: map[string]*float64{"feedbck": nil}})
	if _, err := e.Publish(ctx, match); err != nil { // draft and published revision 2
		t.Fatalf("publish: %v", err)
	}
	tags := TagSet{"topic:node.js": 2}
	for name, m := range map[string]TagMerge{
		"stale draft revision":     {ExpectedRevision: 1, ExpectedPublishedRevision: new(int64(2))},
		"stale published revision": {ExpectedRevision: 2, ExpectedPublishedRevision: new(int64(1))},
		"unexpected publication":   {ExpectedRevision: 2},
	} {
		m.ID, m.DraftTags = item.ID, tags
		if _, err := store.MergeTags(ctx, m); !errors.Is(err, ErrConflict) {
			t.Fatalf("%s: err = %v, want ErrConflict", name, err)
		}
	}

	now := time.Now().UTC()
	got, err := store.MergeTags(ctx, TagMerge{
		ID: item.ID, ExpectedRevision: 2, ExpectedPublishedRevision: new(int64(2)),
		DraftTags: tags, PublishedTags: tags, SchemaVersion: "1", SchemaHash: "hash", Now: now,
	})
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if got.Draft.Revision != 3 || got.Published.Revision != 3 || got.State() != StatePublished {
		t.Fatalf("revisions %d/%d state %s: the published snapshot must follow the bump", got.Draft.Revision, got.Published.Revision, got.State())
	}
	if !maps.Equal(got.Draft.Tags, tags) || !maps.Equal(got.Published.Tags, tags) || got.Published.Fields["title"]["de"] != "v1" {
		t.Fatalf("merged item = draft %+v, published %+v", got.Draft, got.Published)
	}
	if !got.Published.UpdatedAt.Equal(now.Truncate(time.Millisecond)) || got.Draft.SchemaHash != "hash" {
		t.Fatalf("published updatedAt %v, draft schema hash %q", got.Published.UpdatedAt, got.Draft.SchemaHash)
	}
	var raw bson.M
	if err := store.coll.FindOne(ctx, bson.D{{Key: "_id", Value: item.ID}}).Decode(&raw); err != nil {
		t.Fatalf("raw: %v", err)
	}
	if _, ok := raw["published"].(bson.M)["tags"].(bson.M)["topic:node%2Ejs"]; !ok {
		t.Fatalf("published tags must be stored escaped: %v", raw["published"])
	}

	// With pending edits the published revision stays, and nil tags leave a snapshot alone.
	if _, err := e.SaveDraft(ctx, SaveDraftInput{Match: match, Fields: titleFields("v2")}); err != nil { // revision 4
		t.Fatalf("save draft: %v", err)
	}
	publishedAt := got.Published.UpdatedAt
	got, err = store.MergeTags(ctx, TagMerge{ID: item.ID, ExpectedRevision: 4, ExpectedPublishedRevision: new(int64(3)), DraftTags: TagSet{"go": 1}, Now: time.Now().UTC()})
	if err != nil {
		t.Fatalf("merge with pending edits: %v", err)
	}
	if got.Draft.Revision != 5 || got.Published.Revision != 3 || got.State() != StateChanged ||
		!maps.Equal(got.Published.Tags, tags) || !got.Published.UpdatedAt.Equal(publishedAt) {
		t.Fatalf("draft %+v, published %+v", got.Draft, got.Published)
	}

	// An unpublished item matches "no published snapshot"; an archived one never matches.
	draft := newArticle(t, e)
	if _, err := store.MergeTags(ctx, TagMerge{ID: draft.ID, ExpectedRevision: 1, DraftTags: tags, Now: now}); err != nil {
		t.Fatalf("merge into a draft: %v", err)
	}
	if _, err := e.Archive(ctx, ContentItemMatch{ID: draft.ID}); err != nil {
		t.Fatalf("archive: %v", err)
	}
	if _, err := store.MergeTags(ctx, TagMerge{ID: draft.ID, ExpectedRevision: 2, DraftTags: tags, Now: now}); !errors.Is(err, ErrConflict) {
		t.Fatalf("archived item: err = %v, want ErrConflict", err)
	}
}

func TestMergeTagsInMongo(t *testing.T) {
	ctx := context.Background()
	e, _ := newMongoTreeEngine(t, WithTagIndexes())
	var published []string
	for i := range 4 {
		item := updateTags(t, e, UpdateTagsInput{Match: ContentItemMatch{ID: newArticle(t, e).ID}, Set: map[string]*float64{
			"topic:databse": new(float64(i)), "go": nil,
		}})
		if i%2 == 0 {
			if _, err := e.Publish(ctx, ContentItemMatch{ID: item.ID}); err != nil {
				t.Fatalf("publish: %v", err)
			}
			published = append(published, item.ID)
		}
	}
	res, err := e.MergeTags(ctx, MergeTagsInput{From: "topic:databse", To: "topic:database"})
	if err != nil || res.Merged != 4 || len(res.Failed) != 0 {
		t.Fatalf("merge = %+v, %v", res, err)
	}
	for view, want := range map[SnapshotView][]TagCount{
		SnapshotViewPreview:   {{ID: "go", Count: 4}, {ID: "topic:database", Count: 4}},
		SnapshotViewPublished: {{ID: "go", Count: 2}, {ID: "topic:database", Count: 2}},
	} {
		counts, err := e.GetTagCounts(ctx, view, TagCountsInput{})
		if err != nil || !slices.Equal(counts.Items, want) {
			t.Fatalf("%s tag counts = %+v, %v; want %+v", view, counts, err, want)
		}
	}
	page, err := e.ListTagged(ctx, SnapshotViewPublished, ListTaggedInput{Tag: "topic:database"})
	if err != nil || len(page.Items) != 2 || page.Items[0].ID != published[1] || page.Items[0].Tags["topic:database"] != 2 {
		t.Fatalf("published ranking = %+v, %v", page, err)
	}
	for _, id := range published {
		if got := getItem(t, e, id); got.State() != StatePublished {
			t.Fatalf("item %s state = %s, want published", id, got.State())
		}
	}
}
