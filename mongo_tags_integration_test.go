package contentstore

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"testing"

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
