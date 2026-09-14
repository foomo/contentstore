package contentstore

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

const localMongoEndpoint = "mongodb://localhost:27017/"

func TestUnarchiveClearsArchivedAtInMongo(t *testing.T) {
	ctx := context.Background()
	store, err := NewMongoStore(ctx, mongoURLForTest(t))
	if err != nil {
		t.Fatalf("NewMongoStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	engine := newTestEngine(store)
	key := "cms:category:archive-test:article:primary"
	seedPublishedIntegrationItem(t, engine, key)

	if _, err := engine.Archive(ctx, ContentItemMatch{Key: key}); err != nil {
		t.Fatalf("archive: %v", err)
	}
	got, err := engine.GetItem(ctx, ContentItemMatch{Key: key})
	if err != nil {
		t.Fatalf("get after archive: %v", err)
	}
	if got.ArchivedAt == nil || got.State() != StateArchived {
		t.Fatalf("expected archived, got archivedAt=%v state=%s", got.ArchivedAt, got.State())
	}

	if _, err := engine.Unarchive(ctx, ContentItemMatch{Key: key}); err != nil {
		t.Fatalf("unarchive: %v", err)
	}
	got, err = engine.GetItem(ctx, ContentItemMatch{Key: key})
	if err != nil {
		t.Fatalf("get after unarchive: %v", err)
	}
	if got.ArchivedAt != nil {
		t.Fatalf("unarchive must clear archivedAt, got %v", got.ArchivedAt)
	}
	if got.Published != nil {
		t.Fatalf("unarchive must drop the published snapshot, got %#v", got.Published)
	}
	if got.State() != StateDraft {
		t.Fatalf("expected draft state after unarchive, got %s", got.State())
	}

	assertNotPublished(t, engine, key)
}

func TestUnpublishClearsPublishedInMongo(t *testing.T) {
	ctx := context.Background()
	store, err := NewMongoStore(ctx, mongoURLForTest(t))
	if err != nil {
		t.Fatalf("NewMongoStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	engine := newTestEngine(store)
	key := "cms:category:unpublish-test:article:primary"
	seedPublishedIntegrationItem(t, engine, key)

	if _, err := engine.Unpublish(ctx, ContentItemMatch{Key: key}); err != nil {
		t.Fatalf("unpublish: %v", err)
	}
	got, err := engine.GetItem(ctx, ContentItemMatch{Key: key})
	if err != nil {
		t.Fatalf("get after unpublish: %v", err)
	}
	if got.Published != nil {
		t.Fatalf("unpublish must clear the stored published snapshot, got %#v", got.Published)
	}
	if got.ArchivedAt != nil {
		t.Fatalf("unpublish must not archive the item, got %v", got.ArchivedAt)
	}
	if got.Draft == nil || got.State() != StateDraft {
		t.Fatalf("expected a preserved draft in draft state, got draft=%#v state=%s", got.Draft, got.State())
	}

	assertNotPublished(t, engine, key)
}

func TestMongoStoreReplaceAllSwapsCollection(t *testing.T) {
	ctx := context.Background()
	store, err := NewMongoStore(ctx, mongoURLForTest(t))
	if err != nil {
		t.Fatalf("NewMongoStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	now := time.Now().UTC().Truncate(time.Millisecond)
	old := persistedTransferTestItem("old-id", "old-key", now)
	if err := store.Upsert(ctx, old, nil); err != nil {
		t.Fatalf("seed old item: %v", err)
	}
	newItems := []*ContentItem{
		persistedTransferTestItem("new-id-a", "new-key-a", now),
		persistedTransferTestItem("new-id-b", "new-key-b", now),
	}
	if err := store.ReplaceAll(ctx, newItems); err != nil {
		t.Fatalf("ReplaceAll: %v", err)
	}

	if _, err := store.GetByID(ctx, old.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("old item lookup error = %v, want ErrNotFound", err)
	}
	for _, want := range newItems {
		got, err := store.GetByID(ctx, want.ID)
		if err != nil {
			t.Fatalf("GetByID(%q): %v", want.ID, err)
		}
		if got.Key != want.Key || got.Type != want.Type || got.Draft == nil || got.Draft.Revision != want.Draft.Revision {
			t.Fatalf("GetByID(%q) = %#v, want preserved identity and draft", want.ID, got)
		}
	}
}

func seedPublishedIntegrationItem(t *testing.T, engine *Engine, key string) {
	t.Helper()
	ctx := context.Background()
	if _, err := engine.SaveDraft(ctx, SaveDraftInput{
		Match:    ContentItemMatch{Key: key},
		Type:     testType,
		OwnerRef: &OwnerRef{Source: "cms", Type: "category", ID: "category-1"},
		Fields:   validFields(),
	}); err != nil {
		t.Fatalf("seed draft: %v", err)
	}
	if _, err := engine.Publish(ctx, ContentItemMatch{Key: key}); err != nil {
		t.Fatalf("publish: %v", err)
	}
}

func assertNotPublished(t *testing.T, engine *Engine, key string) {
	t.Helper()
	snapshot, err := engine.GetSnapshot(context.Background(), SnapshotViewPublished, SnapshotInput{})
	if err != nil {
		t.Fatalf("published snapshot: %v", err)
	}
	for _, item := range snapshot.Items {
		if item.Key == key {
			t.Fatalf("item %q must not be in the published snapshot", key)
		}
	}
}

func persistedTransferTestItem(id, key string, now time.Time) *ContentItem {
	return &ContentItem{
		ID:        id,
		Key:       key,
		Type:      testType,
		CreatedAt: now,
		UpdatedAt: now,
		Draft: &ContentSnapshot{
			Revision:      3,
			SchemaVersion: "1",
			SchemaHash:    "schema-hash",
			Fields: ContentFields{
				"metaTitle": {"de": "Example"},
			},
			CreatedAt: now,
			UpdatedAt: now,
		},
	}
}

func mongoURLForTest(t *testing.T) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	client, err := mongo.Connect(options.Client().ApplyURI(localMongoEndpoint + "test").SetConnectTimeout(100 * time.Millisecond))
	if err != nil {
		t.Skipf("MongoDB is unavailable: %v", err)
	}
	if err := client.Ping(ctx, nil); err != nil {
		_ = client.Disconnect(context.Background())
		t.Skipf("MongoDB is unavailable: %v", err)
	}
	if err := client.Disconnect(ctx); err != nil {
		t.Fatalf("disconnect MongoDB probe: %v", err)
	}

	name := regexp.MustCompile(`[^a-zA-Z0-9_]`).ReplaceAllString(t.Name(), "_")
	if len(name) > 40 {
		name = name[:40]
	}
	name = fmt.Sprintf("contentstore_%s_%d", name, time.Now().UnixNano())
	t.Cleanup(func() {
		cleanupClient, err := mongo.Connect(options.Client().ApplyURI(localMongoEndpoint + name))
		if err != nil {
			t.Errorf("connect for MongoDB cleanup: %v", err)
			return
		}
		defer func() {
			if err := cleanupClient.Disconnect(context.Background()); err != nil {
				t.Errorf("disconnect after MongoDB cleanup: %v", err)
			}
		}()
		if err := cleanupClient.Database(name).Drop(context.Background()); err != nil {
			t.Errorf("drop MongoDB test database: %v", err)
		}
	})
	return localMongoEndpoint + name
}
