package contentstore

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync/atomic"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// newMongoVersionEngine opens an engine over versionRegistry on a fresh database.
func newMongoVersionEngine(t *testing.T, role DeploymentRole, opts ...StoreOption) (*Engine, *Persistor) {
	t.Helper()
	store, err := NewMongoStore(context.Background(), mongoURLForTest(t), opts...)
	if err != nil {
		t.Fatalf("NewMongoStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	var n atomic.Int64
	newID := func() string { return fmt.Sprintf("id-%d", n.Add(1)) }
	return New(Deps{Store: store, Schemas: versionRegistry(), Locales: testLocales, NewID: newID, Role: role}), store
}

// skipUnlessReplicaSet skips tests of tree items on a standalone server.
func skipUnlessReplicaSet(t *testing.T, store *Persistor) {
	t.Helper()
	var hello struct {
		SetName string `bson:"setName"`
	}
	if err := store.client.Database("admin").RunCommand(context.Background(), bson.D{{Key: "hello", Value: 1}}).Decode(&hello); err != nil {
		t.Fatalf("hello: %v", err)
	}
	if hello.SetName == "" {
		t.Skip("MongoDB is not a replica set: tree transactions are unavailable")
	}
}

// storedRevisions returns an item's stored version revisions, oldest first.
func storedRevisions(t *testing.T, store *Persistor, itemID string) []int64 {
	t.Helper()
	versions, err := store.FindVersions(context.Background(), VersionQuery{ItemID: itemID})
	if err != nil {
		t.Fatalf("find versions: %v", err)
	}
	var revisions []int64
	for _, v := range versions {
		if v.Snapshot != nil {
			t.Fatalf("FindVersions returned a snapshot for revision %d", v.Revision)
		}
		revisions = append(revisions, v.Revision)
	}
	slices.Reverse(revisions)
	return revisions
}

func TestVersionsAreOptInInMongo(t *testing.T) {
	ctx := context.Background()
	e, store := newMongoVersionEngine(t, "")
	item := mustSaveDraft(t, e, SaveDraftInput{Type: articleType, Fields: titleFields("v1")})
	_, err := e.Publish(ctx, ContentItemMatch{ID: item.ID})
	wantKind(t, err, KindInternal)
	_, err = e.ListVersions(ctx, ListVersionsInput{Match: ContentItemMatch{ID: item.ID}})
	wantKind(t, err, KindInternal)
	if err := store.InsertVersion(ctx, &ContentVersion{ItemID: item.ID, Revision: 1}); !errors.Is(err, errVersionsDisabled) {
		t.Fatalf("insert without versions = %v", err)
	}
	if deleted, err := e.Delete(ctx, ContentItemMatch{ID: item.ID}); err != nil || !deleted {
		t.Fatalf("delete = %v, %v", deleted, err)
	}
	if names := collectionNames(t, store); slices.Contains(names, collectionVersions) {
		t.Fatalf("a store without versions created their collection: %v", names)
	}
}

func TestTreeVersionsInMongo(t *testing.T) {
	ctx := context.Background()
	e, store := newMongoVersionEngine(t, "", WithVersions())
	skipUnlessReplicaSet(t, store)
	folder := place(t, e, folderType, "", "kb")
	frontMatter := map[string]any{"aliases": []any{"a", "b"}, "nested": map[string]any{"k": "v", "list": []any{map[string]any{"x": "y"}}}}
	doc := mustSaveDraft(t, e, SaveDraftInput{Type: documentType, ParentID: folder.ID, Name: "doc", Fields: mkFields(map[string]map[string]any{
		"title":       {"de": "A"},
		"body":        {"de": "one"},
		"frontMatter": {"de": frontMatter},
	})})
	match := ContentItemMatch{ID: doc.ID}
	// Tag IDs that are escaped in MongoDB.
	tags := TagSet{"topic:node.js": 2, "2024": 1}
	mustSetTags(t, e, match, map[string]*float64{"topic:node.js": new(2.0), "2024": nil})
	mustPublish(t, WithAuthor(ctx, "ann"), e, match) // revision 2
	mustSaveDraft(t, e, SaveDraftInput{Match: match, Fields: mkFields(map[string]map[string]any{
		"title":       {"de": "B"},
		"frontMatter": {"de": map[string]any{"aliases": []any{"a"}}},
	})})
	mustPublish(t, WithAuthor(ctx, "bob"), e, match) // revision 3

	page, err := e.ListVersions(ctx, ListVersionsInput{Match: match})
	if err != nil || len(page.Items) != 2 || page.Items[0].PublishedBy != "bob" || page.Items[1].PublishedBy != "ann" {
		t.Fatalf("list = %+v, %v", page, err)
	}
	v2, err := e.GetVersion(ctx, match, 2)
	if err != nil {
		t.Fatalf("get version: %v", err)
	}
	if !equalValues(v2.Snapshot.Fields["frontMatter"]["de"], frontMatter) || !maps.Equal(v2.Snapshot.Tags, tags) {
		t.Fatalf("version 2 = %+v", v2.Snapshot)
	}
	diff, err := e.CompareVersions(ctx, CompareVersionsInput{Match: match, From: 2, To: 3})
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	var changed []string
	for _, change := range diff.Fields {
		changed = append(changed, change.FieldID)
	}
	if !slices.Equal(changed, []string{"body", "frontMatter", "title"}) || len(diff.Tags) != 0 {
		t.Fatalf("diff = %+v", diff)
	}

	// The restored values were decoded from MongoDB and must validate and store again.
	result, err := e.RestoreVersion(ctx, RestoreVersionInput{Match: match, Revision: 2, ExpectedRevision: new(int64(3))})
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	stored, err := e.GetItem(ctx, match)
	if err != nil {
		t.Fatalf("get item: %v", err)
	}
	for _, item := range []*ContentItem{result.Item, stored} {
		if item.Draft.Revision != 4 || len(diffFields(item.Draft.Fields, v2.Snapshot.Fields)) != 0 || !maps.Equal(item.Draft.Tags, tags) {
			t.Fatalf("restored draft = %+v", item.Draft)
		}
	}
	mustPublish(t, ctx, e, match)
	wantRevisions(t, storedRevisions(t, store, doc.ID), []int64{2, 3, 4})

	// Concurrent publishes of one revision record it once.
	fns := make([]func() error, 5)
	for i := range fns {
		fns[i] = func() error {
			_, err := e.Publish(ctx, match)
			return err
		}
	}
	for _, err := range runConcurrently(fns...) {
		if err != nil {
			t.Fatalf("concurrent publish: %v", err)
		}
	}
	wantRevisions(t, storedRevisions(t, store, doc.ID), []int64{2, 3, 4})

	if deleted, err := e.Delete(ctx, match); err != nil || !deleted {
		t.Fatalf("delete = %v, %v", deleted, err)
	}
	wantRevisions(t, storedRevisions(t, store, doc.ID), nil)
}

// TestTreeVersionFailureRollsBackPublishInMongo makes every version insert fail with a
// collection validator: the publish of a tree item must not commit without its version.
func TestTreeVersionFailureRollsBackPublishInMongo(t *testing.T) {
	ctx := context.Background()
	e, store := newMongoVersionEngine(t, "", WithVersions())
	skipUnlessReplicaSet(t, store)
	folder := place(t, e, folderType, "", "kb")
	doc := place(t, e, documentType, folder.ID, "doc")
	match := ContentItemMatch{ID: doc.ID}

	validator := func(rule bson.D) {
		t.Helper()
		err := store.coll.Database().RunCommand(ctx, bson.D{{Key: "collMod", Value: collectionVersions}, {Key: "validator", Value: rule}}).Err()
		if err != nil {
			t.Fatalf("collMod: %v", err)
		}
	}
	validator(bson.D{{Key: "revision", Value: bson.D{{Key: "$lt", Value: 0}}}})
	_, err := e.Publish(ctx, match)
	wantKind(t, err, KindInternal)
	if item, _ := e.GetItem(ctx, match); item.Published != nil {
		t.Fatal("the publish committed without its version")
	}

	validator(bson.D{})
	mustPublish(t, ctx, e, match)
	wantRevisions(t, storedRevisions(t, store, doc.ID), []int64{1})
}

// TestVersionsInMongo covers items outside the tree, which need no replica set: an article
// (compare-and-swap writes) and an SEO override (plain writes).
func TestVersionsInMongo(t *testing.T) {
	ctx := context.Background()
	e, store := newMongoVersionEngine(t, DeploymentRoleSandbox, WithVersions())
	article := mustSaveDraft(t, e, SaveDraftInput{Type: articleType, Fields: titleFields("v1")})
	match := ContentItemMatch{ID: article.ID}

	// Concurrent publishes of one revision race on the unique index and record it once.
	fns := make([]func() error, 8)
	for i := range fns {
		fns[i] = func() error {
			_, err := e.Publish(ctx, match)
			return err
		}
	}
	for _, err := range runConcurrently(fns...) {
		if err != nil {
			t.Fatalf("concurrent publish: %v", err)
		}
	}
	wantRevisions(t, storedRevisions(t, store, article.ID), []int64{1})

	for _, title := range []string{"v2", "v3", "v4", "v5"} {
		mustSaveDraft(t, e, SaveDraftInput{Match: match, Fields: titleFields(title)})
		mustPublish(t, ctx, e, match)
	}
	wantRevisions(t, storedRevisions(t, store, article.ID), []int64{3, 4, 5}) // articles keep 3

	seoText := map[string]any{"nodeType": "document", "content": []any{map[string]any{"nodeType": "paragraph"}}}
	seo := mustSaveDraft(t, e, SaveDraftInput{Match: ContentItemMatch{Key: "k1"}, Type: testType, Fields: mkFields(map[string]map[string]any{
		"seoText":         {"de": seoText},
		"filterSelection": {"de": map[string]any{"brand": "nike"}},
	})})
	seoMatch := ContentItemMatch{ID: seo.ID}
	mustPublish(t, ctx, e, seoMatch)
	mustSaveDraft(t, e, SaveDraftInput{Match: seoMatch, Fields: mkFields(map[string]map[string]any{
		"filterSelection": {"de": map[string]any{"brand": "puma"}},
	})})
	result, err := e.RestoreVersion(ctx, RestoreVersionInput{Match: seoMatch, Revision: 1})
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if !equalValues(result.Item.Draft.Fields["seoText"]["de"], seoText) || result.Item.Draft.Fields["filterSelection"]["de"].(map[string]any)["brand"] != "nike" {
		t.Fatalf("restored draft = %+v", result.Item.Draft)
	}

	// Replacing the content clears every version.
	export, err := newContentExport(nil)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if _, err := e.ReplaceContent(ctx, *export); err != nil {
		t.Fatalf("replace content: %v", err)
	}
	if count, err := store.coll.Database().Collection(collectionVersions).CountDocuments(ctx, bson.D{}); err != nil || count != 0 {
		t.Fatalf("versions after replace = %d, %v", count, err)
	}
}
