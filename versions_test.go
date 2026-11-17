package contentstore

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// fakeVersions keeps versions in memory like the persistor's versions collection: one per
// item and revision, unchanged once stored.
type fakeVersions struct {
	vmu      sync.Mutex
	versions map[string]map[int64]*ContentVersion
	// insertErr, when set, fails InsertVersion.
	insertErr error
}

func newFakeVersions() *fakeVersions {
	return &fakeVersions{versions: map[string]map[int64]*ContentVersion{}}
}

func (f *fakeVersions) VersionsEnabled() bool { return true }

func (f *fakeVersions) InsertVersion(_ context.Context, v *ContentVersion) error {
	f.vmu.Lock()
	defer f.vmu.Unlock()
	if f.insertErr != nil {
		return f.insertErr
	}
	byRevision := f.versions[v.ItemID]
	if byRevision == nil {
		byRevision = map[int64]*ContentVersion{}
		f.versions[v.ItemID] = byRevision
	}
	if _, ok := byRevision[v.Revision]; !ok {
		byRevision[v.Revision] = cloneVersion(v)
	}
	return nil
}

func cloneVersion(v *ContentVersion) *ContentVersion {
	clone := *v
	if v.Snapshot != nil {
		snapshot := *v.Snapshot
		snapshot.Tags = maps.Clone(snapshot.Tags)
		clone.Snapshot = &snapshot
	}
	return &clone
}

func (f *fakeVersions) GetVersion(_ context.Context, itemID string, revision int64) (*ContentVersion, error) {
	f.vmu.Lock()
	defer f.vmu.Unlock()
	v, ok := f.versions[itemID][revision]
	if !ok {
		return nil, ErrNotFound
	}
	return cloneVersion(v), nil
}

// newestFirst returns an item's stored revisions, newest first. The caller holds vmu.
func (f *fakeVersions) newestFirst(itemID string) []int64 {
	revisions := slices.Sorted(maps.Keys(f.versions[itemID]))
	slices.Reverse(revisions)
	return revisions
}

func (f *fakeVersions) FindVersions(_ context.Context, q VersionQuery) ([]*ContentVersion, error) {
	f.vmu.Lock()
	defer f.vmu.Unlock()
	var out []*ContentVersion
	for _, revision := range f.newestFirst(q.ItemID) {
		if q.Before > 0 && revision >= q.Before {
			continue
		}
		if q.Limit > 0 && len(out) == q.Limit {
			break
		}
		v := cloneVersion(f.versions[q.ItemID][revision])
		v.Snapshot = nil
		out = append(out, v)
	}
	return out, nil
}

func (f *fakeVersions) PruneVersions(_ context.Context, itemID string, keep int) error {
	f.vmu.Lock()
	defer f.vmu.Unlock()
	revisions := f.newestFirst(itemID)
	for _, revision := range revisions[min(keep, len(revisions)):] {
		delete(f.versions[itemID], revision)
	}
	return nil
}

func (f *fakeVersions) DeleteVersions(_ context.Context, itemID string) error {
	f.vmu.Lock()
	defer f.vmu.Unlock()
	if itemID == "" {
		clear(f.versions)
	} else {
		delete(f.versions, itemID)
	}
	return nil
}

// revisions returns an item's stored revisions, oldest first.
func (f *fakeVersions) revisions(itemID string) []int64 {
	f.vmu.Lock()
	defer f.vmu.Unlock()
	return slices.Sorted(maps.Keys(f.versions[itemID]))
}

// fakeVersionStore adds versions to the in-memory store.
type fakeVersionStore struct {
	*fakeStore
	*fakeVersions
}

func newFakeVersionStore() *fakeVersionStore {
	return &fakeVersionStore{fakeStore: newFakeStore(), fakeVersions: newFakeVersions()}
}

// versionRegistry keeps versions of a plain type (seoOverride), a type that takes tags
// (article, which keeps 3) and a tree type (document); folders keep none.
func versionRegistry() *Registry {
	r := tagRegistry()
	for contentType, keep := range map[ContentType]int{testType: 0, articleType: 3, documentType: 0} {
		schema, _ := r.schemaFor(contentType)
		schema.Versions = &VersionPolicy{Keep: keep}
		r.Register(schema)
	}
	return r
}

func newVersionEngine(store Store, notify Notifier) *Engine {
	return New(Deps{Store: store, Schemas: versionRegistry(), Locales: testLocales, NewID: testNewID(), Notify: notify})
}

func mustPublish(t *testing.T, ctx context.Context, e *Engine, match ContentItemMatch) *ContentItem {
	t.Helper()
	item, err := e.Publish(ctx, match)
	if err != nil {
		t.Fatalf("publish %+v: %v", match, err)
	}
	return item
}

func mustSaveDraft(t *testing.T, e *Engine, input SaveDraftInput) *ContentItem {
	t.Helper()
	item, err := e.SaveDraft(context.Background(), input)
	if err != nil {
		t.Fatalf("save draft: %v", err)
	}
	return item
}

func mustSetTags(t *testing.T, e *Engine, match ContentItemMatch, tags map[string]*float64) {
	t.Helper()
	if _, err := e.UpdateTags(context.Background(), UpdateTagsInput{Match: match, Set: tags, Replace: true}); err != nil {
		t.Fatalf("update tags: %v", err)
	}
}

func wantRevisions(t *testing.T, got, want []int64) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Fatalf("revisions = %v, want %v", got, want)
	}
}

func TestVersionsAreOptIn(t *testing.T) {
	ctx := context.Background()

	// A type that keeps versions is not published over a store without them: its history
	// would be lost.
	e := newVersionEngine(newFakeStore(), nil)
	article := mustSaveDraft(t, e, SaveDraftInput{Type: articleType, Fields: titleFields("v1")})
	_, err := e.Publish(ctx, ContentItemMatch{ID: article.ID})
	wantKind(t, err, KindInternal)
	if got, _ := e.GetItem(ctx, ContentItemMatch{ID: article.ID}); got.Published != nil {
		t.Fatal("the article was published without its version")
	}
	folder := place(t, e, folderType, "", "kb")
	mustPublish(t, ctx, e, ContentItemMatch{ID: folder.ID})
	_, err = e.ListVersions(ctx, ListVersionsInput{Match: ContentItemMatch{ID: article.ID}})
	wantKind(t, err, KindInternal)

	// Over a store with versions, types without a policy record none.
	store := newFakeVersionStore()
	e = newTagEngine(store)
	article = mustSaveDraft(t, e, SaveDraftInput{Type: articleType, Fields: titleFields("v1")})
	mustPublish(t, ctx, e, ContentItemMatch{ID: article.ID})
	if len(store.versions) != 0 {
		t.Fatalf("versions of a type without a policy: %v", store.versions)
	}
}

func TestPublishRecordsVersions(t *testing.T) {
	ctx := context.Background()
	store := newFakeVersionStore()
	e := newVersionEngine(store, nil)
	item := mustSaveDraft(t, e, SaveDraftInput{Type: articleType, Fields: titleFields("v1")})
	match := ContentItemMatch{ID: item.ID}

	first := mustPublish(t, WithAuthor(ctx, "ann"), e, match)
	// Publishing the same revision again keeps its version as it was.
	mustPublish(t, WithAuthor(ctx, "bob"), e, match)
	mustSaveDraft(t, e, SaveDraftInput{Match: match, Fields: titleFields("v2")})
	mustSetTags(t, e, match, map[string]*float64{"topic:go": new(2.0)})
	second := mustPublish(t, WithAuthor(ctx, "bob"), e, match)

	page, err := e.ListVersions(ctx, ListVersionsInput{Match: match})
	if err != nil {
		t.Fatalf("list versions: %v", err)
	}
	if len(page.Items) != 2 || page.NextCursor != "" {
		t.Fatalf("page = %+v", page)
	}
	for i, want := range []struct {
		revision int64
		author   string
	}{{3, "bob"}, {1, "ann"}} {
		got := page.Items[i]
		if got.ItemID != item.ID || got.Revision != want.revision || got.PublishedBy != want.author || got.Snapshot != nil {
			t.Fatalf("version %d = %+v, want revision %d by %q without snapshot", i, got, want.revision, want.author)
		}
	}

	v1, err := e.GetVersion(ctx, match, 1)
	if err != nil {
		t.Fatalf("get version 1: %v", err)
	}
	if !v1.PublishedAt.Equal(first.Published.UpdatedAt) || v1.Snapshot.Fields["title"]["de"] != "v1" {
		t.Fatalf("version 1 = %+v (snapshot %+v)", v1, v1.Snapshot)
	}
	v3, err := e.GetVersion(ctx, match, 3)
	if err != nil {
		t.Fatalf("get version 3: %v", err)
	}
	if !reflect.DeepEqual(v3.Snapshot, second.Published) {
		t.Fatalf("version 3 snapshot = %+v, want the published snapshot %+v", v3.Snapshot, second.Published)
	}

	_, err = e.GetVersion(ctx, match, 2) // a draft revision that was never published
	wantKind(t, err, KindNotFound)
	_, err = e.GetVersion(ctx, match, 0)
	wantKind(t, err, KindBadRequest)
	_, err = e.GetVersion(ctx, ContentItemMatch{ID: "missing"}, 1)
	wantKind(t, err, KindNotFound)
}

func TestListVersionsPages(t *testing.T) {
	ctx := context.Background()
	store := newFakeVersionStore()
	e := newVersionEngine(store, nil)
	folder := place(t, e, folderType, "", "kb")
	doc := place(t, e, documentType, folder.ID, "doc")
	match := ContentItemMatch{ID: doc.ID}
	mustPublish(t, ctx, e, match)
	for _, title := range []string{"v2", "v3", "v4", "v5"} {
		mustSaveDraft(t, e, SaveDraftInput{Match: match, Fields: titleFields(title)})
		mustPublish(t, ctx, e, match)
	}

	var got []int64
	cursor := ""
	for range 3 {
		page, err := e.ListVersions(ctx, ListVersionsInput{Match: match, Limit: 2, Cursor: cursor})
		if err != nil {
			t.Fatalf("list versions: %v", err)
		}
		for _, v := range page.Items {
			got = append(got, v.Revision)
		}
		cursor = page.NextCursor
	}
	wantRevisions(t, got, []int64{5, 4, 3, 2, 1})
	if cursor != "" {
		t.Fatalf("last page has cursor %q", cursor)
	}

	for _, input := range []ListVersionsInput{
		{Match: match, Cursor: "x"},
		{Match: match, Cursor: "0"},
		{Match: match, Limit: -1},
		{Match: match, Limit: maxChildrenLimit + 1},
		{},
	} {
		_, err := e.ListVersions(ctx, input)
		wantKind(t, err, KindBadRequest)
	}
}

func TestPublishPrunesVersions(t *testing.T) {
	ctx := context.Background()
	store := newFakeVersionStore()
	e := newVersionEngine(store, nil)
	item := mustSaveDraft(t, e, SaveDraftInput{Type: articleType, Fields: titleFields("v1")})
	match := ContentItemMatch{ID: item.ID}
	mustPublish(t, ctx, e, match)
	for _, title := range []string{"v2", "v3", "v4", "v5"} {
		mustSaveDraft(t, e, SaveDraftInput{Match: match, Fields: titleFields(title)})
		mustPublish(t, ctx, e, match)
	}
	wantRevisions(t, store.revisions(item.ID), []int64{3, 4, 5}) // articles keep 3
}

// TestPublishRecordsReplacedSnapshot covers published snapshots that were never recorded:
// one published before the type kept versions, and one a tag merge rewrote.
func TestPublishRecordsReplacedSnapshot(t *testing.T) {
	ctx := context.Background()
	store := newFakeVersionStore()
	before := newTagEngine(store) // the type keeps no versions yet
	folder := place(t, before, folderType, "", "kb")
	doc := place(t, before, documentType, folder.ID, "doc")
	match := ContentItemMatch{ID: doc.ID}
	old := mustPublish(t, ctx, before, match)

	e := newVersionEngine(store, nil)
	mustSaveDraft(t, e, SaveDraftInput{Match: match, Fields: titleFields("v2")})
	mustPublish(t, WithAuthor(ctx, "ann"), e, match)
	wantRevisions(t, store.revisions(doc.ID), []int64{1, 2})
	v1, err := e.GetVersion(ctx, match, 1)
	if err != nil {
		t.Fatalf("get version 1: %v", err)
	}
	if v1.PublishedBy != "" || !v1.PublishedAt.Equal(old.Published.UpdatedAt) || v1.Snapshot.Fields["title"]["de"] != "doc" {
		t.Fatalf("baseline version = %+v (snapshot %+v)", v1, v1.Snapshot)
	}

	mustSetTags(t, e, match, map[string]*float64{"topic:go": new(2.0)})
	mustPublish(t, ctx, e, match)
	if _, err := e.MergeTags(ctx, MergeTagsInput{From: "topic:go", To: "topic:golang"}); err != nil {
		t.Fatalf("merge tags: %v", err)
	}
	mustSaveDraft(t, e, SaveDraftInput{Match: match, Fields: titleFields("v3")})
	mustPublish(t, ctx, e, match)
	wantRevisions(t, store.revisions(doc.ID), []int64{1, 2, 3, 4, 5})
	merged, err := e.GetVersion(ctx, match, 4)
	if err != nil {
		t.Fatalf("get version 4: %v", err)
	}
	if !maps.Equal(merged.Snapshot.Tags, TagSet{"topic:golang": 2}) {
		t.Fatalf("merged version tags = %v", merged.Snapshot.Tags)
	}
}

func TestPublishVersionWriteFailure(t *testing.T) {
	ctx := context.Background()
	store := newFakeVersionStore()
	notifier := &recordingNotifier{}
	e := newVersionEngine(store, notifier.record)
	item := mustSaveDraft(t, e, SaveDraftInput{Type: articleType, Fields: titleFields("v1")})
	match := ContentItemMatch{ID: item.ID}

	// Outside the tree the item is published first: it stays published and reports it, and
	// the error says the version is missing.
	store.insertErr = errors.New("versions unavailable")
	_, err := e.Publish(ctx, match)
	wantKind(t, err, KindInternal)
	if got, _ := e.GetItem(ctx, match); got.State() != StatePublished {
		t.Fatalf("state = %s, want published", got.State())
	}
	if last := notifier.changes[len(notifier.changes)-1]; last.Op != OpPublish {
		t.Fatalf("last change = %+v, want a publish", last)
	}

	// Publishing again records it.
	store.insertErr = nil
	mustPublish(t, ctx, e, match)
	wantRevisions(t, store.revisions(item.ID), []int64{1})
}

func TestCompareVersionFields(t *testing.T) {
	ctx := context.Background()
	store := newFakeVersionStore()
	e := newVersionEngine(store, nil)
	item := mustSaveDraft(t, e, SaveDraftInput{Match: ContentItemMatch{Key: "k1"}, Type: testType, Fields: mkFields(map[string]map[string]any{
		"metaTitle":       {"de": "Titel", "fr": "Titre"},
		"ranking":         {"de": int64(1)},
		"filterSelection": {"de": map[string]any{"brand": "nike"}},
	})})
	match := ContentItemMatch{ID: item.ID}
	mustPublish(t, ctx, e, match)
	mustSaveDraft(t, e, SaveDraftInput{Match: match, Fields: mkFields(map[string]map[string]any{
		"metaTitle":       {"de": "Neuer Titel"},
		"ranking":         {"de": float64(1)}, // the same number
		"seoText":         {"de": map[string]any{"nodeType": "document"}},
		"filterSelection": {"de": map[string]any{"brand": "nike"}},
	})})
	mustPublish(t, ctx, e, match)

	diff, err := e.CompareVersions(ctx, CompareVersionsInput{Match: match, From: 1, To: 2})
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	want := []FieldChange{
		{FieldID: "metaTitle", Locale: "de", From: "Titel", To: "Neuer Titel"},
		{FieldID: "metaTitle", Locale: "fr", From: "Titre"},
		{FieldID: "seoText", Locale: "de", To: map[string]any{"nodeType": "document"}},
	}
	if diff.From != 1 || diff.To != 2 || !reflect.DeepEqual(diff.Fields, want) || len(diff.Tags) != 0 {
		t.Fatalf("diff = %+v, want fields %+v", diff, want)
	}

	// 0 is the current draft.
	if _, err := e.SetFieldValue(ctx, SetFieldValueInput{Match: match, FieldID: "metaDescription", Locale: "de", Value: "Text"}); err != nil {
		t.Fatalf("set field: %v", err)
	}
	diff, err = e.CompareVersions(ctx, CompareVersionsInput{Match: match, From: 2})
	if err != nil {
		t.Fatalf("compare with draft: %v", err)
	}
	if diff.To != 3 || !reflect.DeepEqual(diff.Fields, []FieldChange{{FieldID: "metaDescription", Locale: "de", To: "Text"}}) {
		t.Fatalf("diff with draft = %+v", diff)
	}

	_, err = e.CompareVersions(ctx, CompareVersionsInput{Match: match, From: 99})
	wantKind(t, err, KindNotFound)
	_, err = e.CompareVersions(ctx, CompareVersionsInput{Match: match, From: -1})
	wantKind(t, err, KindBadRequest)
}

func TestCompareVersionTags(t *testing.T) {
	ctx := context.Background()
	store := newFakeVersionStore()
	// The item was published before its type kept versions: its published revision is
	// compared from the item itself.
	before := newTagEngine(store)
	item := mustSaveDraft(t, before, SaveDraftInput{Type: articleType, Fields: titleFields("v1")})
	match := ContentItemMatch{ID: item.ID}
	mustSetTags(t, before, match, map[string]*float64{"go": nil, "topic:db": new(2.0), "audience:editors": nil})
	mustPublish(t, ctx, before, match)
	mustSetTags(t, before, match, map[string]*float64{"go": nil, "topic:db": new(3.0), "topic:go": nil})

	e := newVersionEngine(store, nil)
	diff, err := e.CompareVersions(ctx, CompareVersionsInput{Match: match, From: 2, To: 0})
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	want := []TagChange{
		{ID: "audience:editors", From: new(1.0)},
		{ID: "topic:db", From: new(2.0), To: new(3.0)},
		{ID: "topic:go", To: new(1.0)},
	}
	if diff.From != 2 || diff.To != 3 || len(diff.Fields) != 0 || !reflect.DeepEqual(diff.Tags, want) {
		t.Fatalf("diff = %+v, want tags %+v", diff, want)
	}
	if b, _ := json.Marshal(diff); !strings.Contains(string(b), `"fields":[]`) {
		t.Fatalf("an empty diff list must encode as []: %s", b)
	}
}

func TestRestoreVersion(t *testing.T) {
	ctx := context.Background()
	store := newFakeVersionStore()
	notifier := &recordingNotifier{}
	e := newVersionEngine(store, notifier.record)
	kb := place(t, e, folderType, "", "kb")
	other := place(t, e, folderType, "", "other")
	doc := mustSaveDraft(t, e, SaveDraftInput{Type: documentType, ParentID: kb.ID, Name: "doc", Fields: mkFields(map[string]map[string]any{
		"title": {"de": "A"},
		"body":  {"de": "one"},
	})})
	match := ContentItemMatch{ID: doc.ID}
	mustSetTags(t, e, match, map[string]*float64{"topic:db": new(2.0)})
	mustPublish(t, ctx, e, match) // revision 2
	mustSaveDraft(t, e, SaveDraftInput{Match: match, Fields: titleFields("B")})
	mustSetTags(t, e, match, map[string]*float64{"go": nil})
	mustPublish(t, ctx, e, match) // revision 4
	if _, err := e.Move(ctx, MoveInput{Match: match, ParentID: &other.ID}); err != nil {
		t.Fatalf("move: %v", err)
	}
	notifier.changes = nil

	result, err := e.RestoreVersion(ctx, RestoreVersionInput{Match: match, Revision: 2})
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	item := result.Item
	if item.Draft.Revision != 5 || item.Draft.Fields["title"]["de"] != "A" || item.Draft.Fields["body"]["de"] != "one" ||
		!maps.Equal(item.Draft.Tags, TagSet{"topic:db": 2}) {
		t.Fatalf("restored draft = %+v", item.Draft)
	}
	if item.State() != StateChanged || item.Published.Revision != 4 || item.ParentID != other.ID {
		t.Fatalf("restored item: state %s, published %d, parent %q", item.State(), item.Published.Revision, item.ParentID)
	}
	if len(result.DroppedFields) != 0 || len(result.DroppedTags) != 0 {
		t.Fatalf("dropped %+v, %v", result.DroppedFields, result.DroppedTags)
	}
	if len(notifier.changes) != 1 || notifier.changes[0].Op != OpSaveDraft {
		t.Fatalf("changes = %+v, want one saveDraft", notifier.changes)
	}

	// Restoring the content the draft already has changes nothing.
	result, err = e.RestoreVersion(ctx, RestoreVersionInput{Match: match, Revision: 2})
	if err != nil || result.Item.Draft.Revision != 5 || len(notifier.changes) != 1 {
		t.Fatalf("repeated restore: revision %d, %d changes, %v", result.Item.Draft.Revision, len(notifier.changes), err)
	}

	// ExpectedRevision guards the draft; the published revision restores too.
	_, err = e.RestoreVersion(ctx, RestoreVersionInput{Match: match, Revision: 4, ExpectedRevision: new(int64(4))})
	wantKind(t, err, KindConflict)
	result, err = e.RestoreVersion(ctx, RestoreVersionInput{Match: match, Revision: 4, ExpectedRevision: new(int64(5))})
	if err != nil {
		t.Fatalf("restore the published revision: %v", err)
	}
	if result.Item.Draft.Revision != 6 || result.Item.Draft.Fields["title"]["de"] != "B" || result.Item.Draft.Fields["body"] != nil {
		t.Fatalf("draft after restoring the published revision = %+v", result.Item.Draft)
	}

	// Publishing a restored draft records it as a new version.
	mustPublish(t, ctx, e, match)
	wantRevisions(t, store.revisions(doc.ID), []int64{2, 4, 6})

	for _, input := range []RestoreVersionInput{
		{Match: match},
		{Match: match, Revision: 2, ExpectedRevision: new(int64(0))},
	} {
		_, err = e.RestoreVersion(ctx, input)
		wantKind(t, err, KindBadRequest)
	}
	_, err = e.RestoreVersion(ctx, RestoreVersionInput{Match: match, Revision: 3})
	wantKind(t, err, KindNotFound)
	if _, err := e.Archive(ctx, match); err != nil {
		t.Fatalf("archive: %v", err)
	}
	_, err = e.RestoreVersion(ctx, RestoreVersionInput{Match: match, Revision: 2})
	wantKind(t, err, KindNotAcceptable)
}

func TestRestoreVersionGuardsConcurrentWrites(t *testing.T) {
	ctx := context.Background()
	store := newFakeVersionStore()
	e := newVersionEngine(store, nil)
	item := mustSaveDraft(t, e, SaveDraftInput{Match: ContentItemMatch{Key: "k1"}, Type: testType, Fields: mkFields(map[string]map[string]any{
		"metaTitle": {"de": "v1"}, "filterSelection": {"de": map[string]any{"brand": "nike"}},
	})})
	match := ContentItemMatch{ID: item.ID}
	mustPublish(t, ctx, e, match)
	mustSaveDraft(t, e, SaveDraftInput{Match: match, Fields: mkFields(map[string]map[string]any{"metaTitle": {"de": "v2"}})})

	// A write between the restore's read and its write makes the expected revision stale.
	store.beforeWrite = func() {
		store.beforeWrite = nil
		if _, err := e.SetFieldValue(ctx, SetFieldValueInput{Match: match, FieldID: "metaDescription", Locale: "de", Value: "x"}); err != nil {
			t.Errorf("concurrent write: %v", err)
		}
	}
	_, err := e.RestoreVersion(ctx, RestoreVersionInput{Match: match, Revision: 1, ExpectedRevision: new(int64(2))})
	wantKind(t, err, KindConflict)
	got, _ := e.GetItem(ctx, match)
	if got.Draft.Revision != 3 || got.Draft.Fields["metaDescription"]["de"] != "x" {
		t.Fatalf("the concurrent write was lost: %+v", got.Draft)
	}

	// Without an expected revision the restore retries on top of it.
	store.beforeWrite = func() {
		store.beforeWrite = nil
		if _, err := e.SetFieldValue(ctx, SetFieldValueInput{Match: match, FieldID: "metaDescription", Locale: "fr", Value: "y"}); err != nil {
			t.Errorf("concurrent write: %v", err)
		}
	}
	result, err := e.RestoreVersion(ctx, RestoreVersionInput{Match: match, Revision: 1})
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if result.Item.Draft.Revision != 5 || result.Item.Draft.Fields["metaTitle"]["de"] != "v1" {
		t.Fatalf("restored draft = %+v", result.Item.Draft)
	}
}

func TestRestoreVersionDropsWhatTheSchemaNoLongerTakes(t *testing.T) {
	ctx := context.Background()
	const pageType ContentType = "page"
	registry := func(fields []FieldDefinition, tags *TagPolicy) *Registry {
		r := NewRegistry()
		page := NewSchema(pageType, "1", fields)
		page.Tags = tags
		page.Versions = &VersionPolicy{}
		r.Register(page)
		return r
	}
	store := newFakeVersionStore()
	before := New(Deps{Store: store, Locales: testLocales, NewID: testNewID(), Schemas: registry([]FieldDefinition{
		{ID: "title", Type: FieldTypeSymbol, Localized: true},
		{ID: "summary", Type: FieldTypeText, Localized: true},
		{ID: "legacy", Type: FieldTypeSymbol},
	}, &TagPolicy{Namespaces: []TagNamespace{{Name: ""}, {Name: "old"}, {Name: "topic", Weighted: true}}})})
	item := mustSaveDraft(t, before, SaveDraftInput{Type: pageType, Fields: mkFields(map[string]map[string]any{
		"title":   {"de": "Titel", "fr": "Titre"},
		"summary": {"de": "long summary"},
		"legacy":  {"de": "x"},
	})})
	match := ContentItemMatch{ID: item.ID}
	mustSetTags(t, before, match, map[string]*float64{"go": nil, "old:x": nil, "topic:db": new(2.0)})
	mustPublish(t, ctx, before, match) // revision 2

	// The schema then drops legacy and the localization of title; bare tags and the old
	// namespace go, and topics lose their weights.
	fields := []FieldDefinition{
		{ID: "title", Type: FieldTypeSymbol},
		{ID: "summary", Type: FieldTypeText, Localized: true},
	}
	unweighted := &TagPolicy{Namespaces: []TagNamespace{{Name: "topic"}}}
	e := New(Deps{Store: store, Locales: testLocales, NewID: testNewID(), Schemas: registry(fields, unweighted)})
	mustSaveDraft(t, e, SaveDraftInput{Match: match, Fields: ContentFields{}})
	result, err := e.RestoreVersion(ctx, RestoreVersionInput{Match: match, Revision: 2})
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	wantDropped := []FieldLocale{{FieldID: "legacy", Locale: "de"}, {FieldID: "title", Locale: "fr"}}
	if !reflect.DeepEqual(result.DroppedFields, wantDropped) || !slices.Equal(result.DroppedTags, []string{"go", "old:x"}) {
		t.Fatalf("dropped %+v and %v", result.DroppedFields, result.DroppedTags)
	}
	wantFields := mkFields(map[string]map[string]any{"title": {"de": "Titel"}, "summary": {"de": "long summary"}})
	draft := result.Item.Draft
	if !reflect.DeepEqual(draft.Fields, wantFields) || !maps.Equal(draft.Tags, TagSet{"topic:db": 1}) {
		t.Fatalf("restored draft = %+v", draft)
	}
	if schema, _ := e.schemas.schemaFor(pageType); draft.SchemaHash != schema.Hash {
		t.Fatal("the restored draft does not carry the current schema hash")
	}

	// Values the schema now rejects, or more tags than it allows, fail the restore.
	shorter := []FieldDefinition{{ID: "title", Type: FieldTypeSymbol}, {ID: "summary", Type: FieldTypeText, Localized: true, MaxLength: new(4)}}
	e = New(Deps{Store: store, Locales: testLocales, NewID: testNewID(), Schemas: registry(shorter, unweighted)})
	_, err = e.RestoreVersion(ctx, RestoreVersionInput{Match: match, Revision: 2})
	wantKind(t, err, KindBadRequest)
	fewer := &TagPolicy{MaxTags: 1, Namespaces: []TagNamespace{{Name: ""}, {Name: "topic", Weighted: true}}}
	e = New(Deps{Store: store, Locales: testLocales, NewID: testNewID(), Schemas: registry(fields, fewer)})
	_, err = e.RestoreVersion(ctx, RestoreVersionInput{Match: match, Revision: 2})
	wantKind(t, err, KindBadRequest)
}

func TestPlainValue(t *testing.T) {
	decoded := bson.M{
		"nodeType": "document",
		"content":  bson.A{bson.M{"value": "x"}, bson.D{{Key: "marks", Value: bson.A{}}}},
	}
	want := map[string]any{
		"nodeType": "document",
		"content":  []any{map[string]any{"value": "x"}, map[string]any{"marks": []any{}}},
	}
	got := plainValue(decoded)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("plainValue = %#v, want %#v", got, want)
	}
	if err := validateValue(FieldDefinition{Type: FieldTypeRichText}, got); err != nil {
		t.Fatalf("a plain value does not validate: %v", err)
	}
}

func TestVersionCascades(t *testing.T) {
	ctx := context.Background()
	store := newFakeVersionStore()
	e := New(Deps{Store: store, Schemas: versionRegistry(), Locales: testLocales, NewID: testNewID(), Role: DeploymentRoleSandbox})
	item := mustSaveDraft(t, e, SaveDraftInput{Type: articleType, Fields: titleFields("v1")})
	match := ContentItemMatch{ID: item.ID}
	mustPublish(t, ctx, e, match)

	// Archiving and unarchiving keep the history.
	if _, err := e.Archive(ctx, match); err != nil {
		t.Fatalf("archive: %v", err)
	}
	if _, err := e.Unarchive(ctx, match); err != nil {
		t.Fatalf("unarchive: %v", err)
	}
	wantRevisions(t, store.revisions(item.ID), []int64{1})

	// Deleting the item deletes it.
	if deleted, err := e.Delete(ctx, match); err != nil || !deleted {
		t.Fatalf("delete = %v, %v", deleted, err)
	}
	wantRevisions(t, store.revisions(item.ID), nil)

	// Replacing the content clears every version.
	other := mustSaveDraft(t, e, SaveDraftInput{Type: articleType, Fields: titleFields("v1")})
	mustPublish(t, ctx, e, ContentItemMatch{ID: other.ID})
	export, err := newContentExport(nil)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if _, err := e.ReplaceContent(ctx, *export); err != nil {
		t.Fatalf("replace content: %v", err)
	}
	if len(store.versions) != 0 {
		t.Fatalf("versions after replace: %v", store.versions)
	}
}

// fakeScheduleVersionStore keeps scheduled actions and versions.
type fakeScheduleVersionStore struct {
	*fakeScheduleStore
	*fakeVersions
}

func TestScheduledPublishRecordsTheRunAuthor(t *testing.T) {
	ctx := context.Background()
	store := &fakeScheduleVersionStore{fakeScheduleStore: newFakeScheduleStore(), fakeVersions: newFakeVersions()}
	e := newVersionEngine(store, nil)
	item := mustSaveDraft(t, e, SaveDraftInput{Match: ContentItemMatch{Key: "k1"}, Type: articleType, Fields: titleFields("v1")})
	at := scheduleBase()
	mustSchedule(t, e, "k1", ActionPublish, at)
	run, err := e.RunDueActions(WithAuthor(ctx, "scheduler"), at)
	if err != nil || len(run.Actions) != 1 || run.Actions[0].Status != ActionSucceeded {
		t.Fatalf("run = %+v, %v", run, err)
	}
	v, err := e.GetVersion(ctx, ContentItemMatch{ID: item.ID}, 1)
	if err != nil || v.PublishedBy != "scheduler" {
		t.Fatalf("version = %+v, %v", v, err)
	}
}

func TestVersionPolicySchema(t *testing.T) {
	s := NewSchema("page", "1", []FieldDefinition{{ID: "title", Type: FieldTypeSymbol}})
	hash := s.Hash
	s.Versions = &VersionPolicy{Keep: 10}
	if schemaHash(s) != hash {
		t.Fatal("a version policy changed the schema hash")
	}
	b, _ := json.Marshal(s)
	if !strings.Contains(string(b), `"versions":{"keep":10}`) {
		t.Fatalf("schema JSON = %s", b)
	}
	b, _ = json.Marshal(NewSchema("page", "1", nil))
	if strings.Contains(string(b), "versions") {
		t.Fatalf("a schema without a policy encodes one: %s", b)
	}

	defer func() {
		if recover() == nil {
			t.Fatal("a negative keep did not panic")
		}
	}()
	s.Versions.Keep = -1
	NewRegistry().Register(s)
}
