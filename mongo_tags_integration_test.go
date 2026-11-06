package contentstore

import (
	"context"
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
