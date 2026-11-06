package contentstore

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// The tests in this file pin the externally observable shape of content that exists
// today: wire (JSON) and storage (BSON) encodings, export checksums, schema hashes, and
// the exact sequence of store calls each ContentStore operation performs. Their golden
// values were captured from the code before any tree/size-limit work, so any change to
// them means existing consumers or stored data would observe a difference.

var compatTime = time.Date(2026, 1, 2, 3, 4, 5, 6_000_000, time.UTC)

func compatTimePtr() *time.Time {
	t := compatTime.Add(time.Hour)
	return &t
}

func compatSnapshot(revision int64, fields ContentFields) *ContentSnapshot {
	return &ContentSnapshot{
		Revision:      revision,
		SchemaVersion: "1",
		SchemaHash:    "hash-1",
		Fields:        fields,
		CreatedAt:     compatTime,
		UpdatedAt:     compatTime.Add(time.Duration(revision) * time.Minute),
	}
}

// compatJSONItems covers every lifecycle state with multi-key maps (JSON sorts map keys,
// so the encoding is deterministic).
func compatJSONItems() []*ContentItem {
	fields := mkFields(map[string]map[string]any{
		"metaTitle":       {"de": "Titel", "fr": "Titre"},
		"ranking":         {"de": int64(3)},
		"seoText":         {"de": map[string]any{"nodeType": "document", "content": []any{}}},
		"filterSelection": {"de": map[string]any{"brand": "nike", "size": "42"}},
	})
	owner := &OwnerRef{Source: "contentful", Type: "shopCategory", ID: "cat-1"}
	return []*ContentItem{
		{ID: "a-draft", Key: "k-draft", Type: testType, OwnerRef: owner, Draft: compatSnapshot(1, fields), CreatedAt: compatTime, UpdatedAt: compatTime},
		{ID: "b-published", Key: "k-published", Type: testType, Draft: compatSnapshot(3, fields), Published: compatSnapshot(3, fields), CreatedAt: compatTime, UpdatedAt: compatTime},
		{ID: "c-changed", Type: testType, OwnerRef: owner, Draft: compatSnapshot(4, fields), Published: compatSnapshot(3, fields), CreatedAt: compatTime, UpdatedAt: compatTime},
		{ID: "d-archived", Key: "k-archived", Type: testType, Draft: compatSnapshot(2, ContentFields{}), Published: compatSnapshot(2, ContentFields{}), ArchivedAt: compatTimePtr(), CreatedAt: compatTime, UpdatedAt: compatTime},
	}
}

// compatBSONItem uses single-key maps only: the BSON encoder writes Go maps in iteration
// order, so only single-key maps give deterministic bytes.
func compatBSONItem() *ContentItem {
	fields := mkFields(map[string]map[string]any{
		"filterSelection": {"de": map[string]any{"brand": "nike"}},
	})
	return &ContentItem{
		ID: "b-1", Key: "k-1", Type: testType,
		OwnerRef:   &OwnerRef{Source: "contentful", Type: "shopCategory", ID: "cat-1"},
		Draft:      compatSnapshot(4, fields),
		Published:  compatSnapshot(3, fields),
		ArchivedAt: compatTimePtr(),
		CreatedAt:  compatTime, UpdatedAt: compatTime,
	}
}

// compatSEOOverrideSchema mirrors the shape of the production SEO Override schema in the
// globus contentstore service (only the hashed attributes matter).
func compatSEOOverrideSchema() ContentTypeSchema {
	return NewSchema("seoOverride", "1", []FieldDefinition{
		{ID: "metaTitle", Type: FieldTypeSymbol, EditingFormat: FieldEditingFormatPlainText, Localized: true, Label: "SEO meta title", SuggestedMaxLength: new(60)},
		{ID: "metaDescription", Type: FieldTypeSymbol, EditingFormat: FieldEditingFormatPlainText, Localized: true, SuggestedMaxLength: new(160)},
		{ID: "headerHeadline", Type: FieldTypeSymbol, EditingFormat: FieldEditingFormatPlainText, Localized: true},
		{ID: "headerText", Type: FieldTypeSymbol, EditingFormat: FieldEditingFormatPlainText, Localized: true, SuggestedMaxLength: new(240)},
		{ID: "seoText", Type: FieldTypeRichText, EditingFormat: FieldEditingFormatContentfulRichText, Localized: true, FormatHint: "Structure", SuggestedMinLength: new(800), SuggestedMaxLength: new(1800)},
		{ID: "filterSelection", Type: FieldTypeJSON, Localized: false, Required: true, ReadOnly: true, Label: "Filter selection"},
	})
}

// compatWireValues covers the remaining transport-facing shapes: snapshots, the export
// envelope, change events, and every input type in its fully populated form.
func compatWireValues() []any {
	owner := &OwnerRef{Source: "contentful", Type: "shopCategory", ID: "cat-1"}
	fields := mkFields(map[string]map[string]any{"metaTitle": {"de": "Titel"}})
	rev := int64(2)
	return []any{
		Snapshot{Revision: 42, CreatedAt: compatTime, Items: []SnapshotItem{
			{ID: "i1", Key: "k1", Type: testType, OwnerRef: owner, State: StateChanged, Fields: fields, UpdatedAt: compatTime},
			{ID: "i2", Type: testType, State: StatePublished, Fields: ContentFields{}, UpdatedAt: compatTime},
		}},
		ArchivedSnapshot{Revision: 7, CreatedAt: compatTime, Items: []ArchivedContentItem{
			{ID: "i3", Key: "k3", Type: testType, OwnerRef: owner, Fields: fields, ArchivedAt: compatTime, UpdatedAt: compatTime},
		}},
		ContentExport{FormatVersion: ContentExportFormatVersion, CreatedAt: compatTime, ItemCount: 1, Checksum: "sha256:x", Items: compatJSONItems()[:1]},
		ReplaceContentResult{ItemCount: 1, Checksum: "sha256:x"},
		ContentChange{Type: testType, OwnerRef: owner, Op: OpPublish},
		SaveDraftInput{Match: ContentItemMatch{ID: "i1", Key: "k1"}, Type: testType, OwnerRef: owner, Fields: fields, ExpectedRevision: &rev},
		SaveDraftInput{Fields: ContentFields{}},
		SetFieldValueInput{Match: ContentItemMatch{Key: "k1"}, Type: testType, OwnerRef: owner, FieldID: "metaTitle", Locale: "de", Value: "Titel"},
		SnapshotInput{Types: []ContentType{testType}, OwnerRef: owner},
		SnapshotInput{},
		ArchivedSnapshotInput{Types: []ContentType{testType}},
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

// recordingStore wraps the in-memory fake store and logs every Store and TreeStore call
// with the arguments that determine what the call reads or writes. It implements
// TreeStore so the engine could use tree operations: the golden sequence proves it does
// not for existing content types.
type recordingStore struct {
	inner *fakeStore
	calls []string
}

func (r *recordingStore) record(format string, args ...any) {
	r.calls = append(r.calls, fmt.Sprintf(format, args...))
}

func (r *recordingStore) GetByID(ctx context.Context, id string) (*ContentItem, error) {
	r.record("GetByID(%s)", id)
	return r.inner.GetByID(ctx, id)
}

func (r *recordingStore) GetByKey(ctx context.Context, key string) (*ContentItem, error) {
	r.record("GetByKey(%s)", key)
	return r.inner.GetByKey(ctx, key)
}

func (r *recordingStore) Upsert(ctx context.Context, item *ContentItem, expected *int64) error {
	exp := "nil"
	if expected != nil {
		exp = fmt.Sprint(*expected)
	}
	r.record("Upsert(%s,expected=%s)", item.ID, exp)
	return r.inner.Upsert(ctx, item, expected)
}

func (r *recordingStore) SetField(ctx context.Context, in FieldUpdate) (*ContentItem, error) {
	r.record("SetField(id=%s,key=%s,type=%s,field=%s,locale=%s,newID=%s)", in.Match.ID, in.Match.Key, in.Type, in.FieldID, in.Locale, in.NewID)
	return r.inner.SetField(ctx, in)
}

func (r *recordingStore) Delete(ctx context.Context, id string) error {
	r.record("Delete(%s)", id)
	return r.inner.Delete(ctx, id)
}

func (r *recordingStore) FindPublished(ctx context.Context, types []ContentType, ownerRef *OwnerRef) ([]*ContentItem, error) {
	r.record("FindPublished(types=%v,owner=%v)", types, ownerRef != nil)
	return r.inner.FindPublished(ctx, types, ownerRef)
}

func (r *recordingStore) FindAll(ctx context.Context, types []ContentType, ownerRef *OwnerRef) ([]*ContentItem, error) {
	r.record("FindAll(types=%v,owner=%v)", types, ownerRef != nil)
	return r.inner.FindAll(ctx, types, ownerRef)
}

func (r *recordingStore) FindArchived(ctx context.Context, types []ContentType) ([]*ContentItem, error) {
	r.record("FindArchived(types=%v)", types)
	return r.inner.FindArchived(ctx, types)
}

func (r *recordingStore) ExportAll(ctx context.Context) ([]*ContentItem, error) {
	r.record("ExportAll()")
	return r.inner.ExportAll(ctx)
}

func (r *recordingStore) ReplaceAll(ctx context.Context, items []*ContentItem) error {
	r.record("ReplaceAll(%d)", len(items))
	return r.inner.ReplaceAll(ctx, items)
}

func (r *recordingStore) RunTreeTransaction(ctx context.Context, fn func(ctx context.Context) error) error {
	r.record("RunTreeTransaction()")
	return r.inner.RunTreeTransaction(ctx, fn)
}

func (r *recordingStore) FindChildren(ctx context.Context, q ChildQuery) ([]*ContentItem, error) {
	r.record("FindChildren(%+v)", q)
	return r.inner.FindChildren(ctx, q)
}

// compatStep is one ContentStore operation of the legacy-type scenario.
type compatStep struct {
	name string
	run  func(ctx context.Context, e *Engine) error
}

func compatScenario() []compatStep {
	owner := &OwnerRef{Source: "contentful", Type: "shopCategory", ID: "cat-1"}
	rev := func(n int64) *int64 { return &n }
	return []compatStep{
		{"SaveDraft create by key", func(ctx context.Context, e *Engine) error {
			_, err := e.SaveDraft(ctx, SaveDraftInput{Match: ContentItemMatch{Key: "k1"}, Type: testType, OwnerRef: owner, Fields: validFields()})
			return err
		}},
		{"SaveDraft update by key with revision", func(ctx context.Context, e *Engine) error {
			_, err := e.SaveDraft(ctx, SaveDraftInput{Match: ContentItemMatch{Key: "k1"}, Fields: validFields(), ExpectedRevision: rev(1)})
			return err
		}},
		{"SaveDraft create-only", func(ctx context.Context, e *Engine) error {
			_, err := e.SaveDraft(ctx, SaveDraftInput{Match: ContentItemMatch{Key: "k2"}, Type: testType, Fields: validFields(), ExpectedRevision: rev(0)})
			return err
		}},
		{"SaveDraft without match", func(ctx context.Context, e *Engine) error {
			_, err := e.SaveDraft(ctx, SaveDraftInput{Type: testType, Fields: ContentFields{}})
			return err
		}},
		{"SetFieldValue create by key", func(ctx context.Context, e *Engine) error {
			_, err := e.SetFieldValue(ctx, SetFieldValueInput{Match: ContentItemMatch{Key: "k3"}, Type: testType, OwnerRef: owner, FieldID: "metaTitle", Locale: "de", Value: "Titel"})
			return err
		}},
		{"SetFieldValue update by id", func(ctx context.Context, e *Engine) error {
			_, err := e.SetFieldValue(ctx, SetFieldValueInput{Match: ContentItemMatch{ID: "id-1"}, FieldID: "metaTitle", Locale: "fr", Value: "Titre"})
			return err
		}},
		{"SetFieldValue clear by key", func(ctx context.Context, e *Engine) error {
			_, err := e.SetFieldValue(ctx, SetFieldValueInput{Match: ContentItemMatch{Key: "k1"}, FieldID: "metaTitle", Locale: "fr", Value: nil})
			return err
		}},
		{"Publish", func(ctx context.Context, e *Engine) error {
			_, err := e.Publish(ctx, ContentItemMatch{Key: "k1"})
			return err
		}},
		{"Publish missing", func(ctx context.Context, e *Engine) error {
			_, err := e.Publish(ctx, ContentItemMatch{Key: "missing"})
			return err
		}},
		{"GetItem", func(ctx context.Context, e *Engine) error {
			_, err := e.GetItem(ctx, ContentItemMatch{ID: "id-1", Key: "k1"})
			return err
		}},
		{"GetSnapshot published", func(ctx context.Context, e *Engine) error {
			_, err := e.GetSnapshot(ctx, SnapshotViewPublished, SnapshotInput{Types: []ContentType{testType}})
			return err
		}},
		{"GetSnapshot preview by owner", func(ctx context.Context, e *Engine) error {
			_, err := e.GetSnapshot(ctx, SnapshotViewPreview, SnapshotInput{OwnerRef: owner})
			return err
		}},
		{"Unpublish", func(ctx context.Context, e *Engine) error {
			_, err := e.Unpublish(ctx, ContentItemMatch{Key: "k1"})
			return err
		}},
		{"Unpublish not published", func(ctx context.Context, e *Engine) error {
			_, err := e.Unpublish(ctx, ContentItemMatch{Key: "k1"})
			return err
		}},
		{"Archive", func(ctx context.Context, e *Engine) error {
			_, err := e.Archive(ctx, ContentItemMatch{Key: "k1"})
			return err
		}},
		{"GetArchivedSnapshot", func(ctx context.Context, e *Engine) error {
			_, err := e.GetArchivedSnapshot(ctx, ArchivedSnapshotInput{Types: []ContentType{testType}})
			return err
		}},
		{"SaveDraft archived", func(ctx context.Context, e *Engine) error {
			_, err := e.SaveDraft(ctx, SaveDraftInput{Match: ContentItemMatch{Key: "k1"}, Fields: validFields()})
			return err
		}},
		{"Unarchive", func(ctx context.Context, e *Engine) error {
			_, err := e.Unarchive(ctx, ContentItemMatch{Key: "k1"})
			return err
		}},
		{"Delete", func(ctx context.Context, e *Engine) error {
			_, err := e.Delete(ctx, ContentItemMatch{Key: "k1"})
			return err
		}},
		{"Delete missing", func(ctx context.Context, e *Engine) error {
			_, err := e.Delete(ctx, ContentItemMatch{Key: "k1"})
			return err
		}},
		{"GetSchemas", func(ctx context.Context, e *Engine) error {
			_, err := e.GetSchemas(ctx)
			return err
		}},
	}
}

func errorSummary(err error) string {
	if err == nil {
		return "ok"
	}
	if e, ok := errors.AsType[*Error](err); ok {
		return fmt.Sprintf("kind=%d", e.Kind)
	}
	return "error"
}

// compatCallSequence is the store-call sequence of compatScenario captured from the code
// before any tree work: "<step> -> <result> -> <calls>".
var compatCallSequence = []string{
	"SaveDraft create by key -> ok -> GetByKey(k1) Upsert(id-1,expected=nil)",
	"SaveDraft update by key with revision -> ok -> GetByKey(k1) Upsert(id-1,expected=1)",
	"SaveDraft create-only -> ok -> GetByKey(k2) Upsert(id-2,expected=0)",
	"SaveDraft without match -> ok -> Upsert(id-3,expected=nil)",
	"SetFieldValue create by key -> ok -> GetByKey(k3) SetField(id=,key=k3,type=seoOverride,field=metaTitle,locale=de,newID=id-4)",
	"SetFieldValue update by id -> ok -> GetByID(id-1) SetField(id=id-1,key=,type=seoOverride,field=metaTitle,locale=fr,newID=id-5)",
	"SetFieldValue clear by key -> ok -> GetByKey(k1) SetField(id=,key=k1,type=seoOverride,field=metaTitle,locale=fr,newID=id-6)",
	"Publish -> ok -> GetByKey(k1) Upsert(id-1,expected=nil)",
	"Publish missing -> kind=2 -> GetByKey(missing)",
	"GetItem -> ok -> GetByID(id-1) GetByKey(k1)",
	"GetSnapshot published -> ok -> FindPublished(types=[seoOverride],owner=false)",
	"GetSnapshot preview by owner -> ok -> FindAll(types=[],owner=true)",
	"Unpublish -> ok -> GetByKey(k1) Upsert(id-1,expected=nil)",
	"Unpublish not published -> ok -> GetByKey(k1)",
	"Archive -> ok -> GetByKey(k1) Upsert(id-1,expected=nil)",
	"GetArchivedSnapshot -> ok -> FindArchived(types=[seoOverride])",
	"SaveDraft archived -> kind=3 -> GetByKey(k1)",
	"Unarchive -> ok -> GetByKey(k1) Upsert(id-1,expected=nil)",
	"Delete -> ok -> GetByKey(k1) Delete(id-1)",
	"Delete missing -> ok -> GetByKey(k1)",
	"GetSchemas -> ok -> ",
}

const (
	compatChecksum       = "sha256:c2ba48b63c5e1fbb3c04b8d61c5e28358882329e6245177f7552138a4b8fc2fc"
	compatTestSchemaHash = "63747812305660189e0c528214509af1512afb19749c923b0870497880bc60d7"
	compatSEOSchemaHash  = "e4a6e40c309f00ae863b36cf1c33a938c1e7256a9a03e3b2921e2d36d9639270"
)

func readGolden(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "compat", name))
	if err != nil {
		t.Fatalf("read golden %s: %v", name, err)
	}
	return strings.TrimSpace(string(b))
}

func TestCompatContentItemJSON(t *testing.T) {
	if got, want := mustJSON(t, compatJSONItems()), readGolden(t, "items.json"); got != want {
		t.Fatalf("content item JSON changed\ngot:  %s\nwant: %s", got, want)
	}
}

func TestCompatContentItemBSON(t *testing.T) {
	item := compatBSONItem()
	b, err := bson.Marshal(item)
	if err != nil {
		t.Fatalf("bson marshal: %v", err)
	}
	if got, want := hex.EncodeToString(b), readGolden(t, "item.bson.hex"); got != want {
		t.Fatalf("content item BSON changed\ngot:  %s\nwant: %s", got, want)
	}

	// A document stored by the current code decodes to the same item.
	raw, err := hex.DecodeString(readGolden(t, "item.bson.hex"))
	if err != nil {
		t.Fatalf("decode golden hex: %v", err)
	}
	var decoded ContentItem
	if err := bson.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("bson unmarshal: %v", err)
	}
	decoded.Draft.Fields["filterSelection"]["de"] = bsonDocToMap(t, decoded.Draft.Fields["filterSelection"]["de"])
	decoded.Published.Fields["filterSelection"]["de"] = bsonDocToMap(t, decoded.Published.Fields["filterSelection"]["de"])
	if got, want := mustJSON(t, decoded), mustJSON(t, item); got != want {
		t.Fatalf("stored document decodes differently\ngot:  %s\nwant: %s", got, want)
	}
}

// bsonDocToMap normalizes an embedded document decoded without the persistor's registry
// (bson.D) into the map form the persistor produces.
func bsonDocToMap(t *testing.T, v any) map[string]any {
	t.Helper()
	d, ok := v.(bson.D)
	if !ok {
		t.Fatalf("expected bson.D, got %T", v)
	}
	out := map[string]any{}
	for _, e := range d {
		out[e.Key] = e.Value
	}
	return out
}

func TestCompatExportChecksum(t *testing.T) {
	got, err := contentItemsChecksum(compatJSONItems())
	if err != nil {
		t.Fatalf("checksum: %v", err)
	}
	if got != compatChecksum {
		t.Fatalf("export checksum changed: got %s, want %s", got, compatChecksum)
	}
	export, err := newContentExport(compatJSONItems())
	if err != nil {
		t.Fatalf("newContentExport: %v", err)
	}
	if export.Checksum != compatChecksum || export.FormatVersion != 1 {
		t.Fatalf("export envelope changed: version=%d checksum=%s", export.FormatVersion, export.Checksum)
	}
}

func TestCompatSchemaHashes(t *testing.T) {
	if got := testRegistry().schemas[testType].Hash; got != compatTestSchemaHash {
		t.Fatalf("test schema hash changed: got %s, want %s", got, compatTestSchemaHash)
	}
	if got := compatSEOOverrideSchema().Hash; got != compatSEOSchemaHash {
		t.Fatalf("seoOverride-shaped schema hash changed: got %s, want %s", got, compatSEOSchemaHash)
	}
	reg := NewRegistry()
	reg.Register(compatSEOOverrideSchema())
	res, err := New(Deps{Schemas: reg}).GetSchemas(context.Background())
	if err != nil {
		t.Fatalf("GetSchemas: %v", err)
	}
	if got, want := mustJSON(t, res), readGolden(t, "schemas.json"); got != want {
		t.Fatalf("schemas JSON changed\ngot:  %s\nwant: %s", got, want)
	}
}

// TestCompatWireShapes pins the encoding of every transport-facing value and proves that
// payloads produced by current clients decode and re-encode without loss or additions.
func TestCompatWireShapes(t *testing.T) {
	values := compatWireValues()
	want := readGolden(t, "wire.json")
	if got := mustJSON(t, values); got != want {
		t.Fatalf("wire JSON changed\ngot:  %s\nwant: %s", got, want)
	}
	var raws []json.RawMessage
	if err := json.Unmarshal([]byte(want), &raws); err != nil {
		t.Fatalf("unmarshal golden: %v", err)
	}
	for i, v := range values {
		ptr := reflect.New(reflect.TypeOf(v))
		if err := json.Unmarshal(raws[i], ptr.Interface()); err != nil {
			t.Fatalf("value %d (%T): unmarshal: %v", i, v, err)
		}
		if got := mustJSON(t, ptr.Elem().Interface()); got != string(raws[i]) {
			t.Fatalf("value %d (%T) does not round-trip\ngot:  %s\nwant: %s", i, v, got, raws[i])
		}
	}
}

// TestCompatStoreCallSequence proves every ContentStore operation on an existing (non-tree)
// content type performs exactly the store calls it performed before the tree work.
func TestCompatStoreCallSequence(t *testing.T) {
	ctx := context.Background()
	rec := &recordingStore{inner: newFakeStore()}
	var _ TreeStore = rec
	// Tree and tagged types are registered next to the legacy type, as a knowledge base would.
	e := New(Deps{Store: rec, Schemas: tagRegistry(), Locales: testLocales, NewID: testNewID()})
	var got []string
	for _, step := range compatScenario() {
		rec.calls = nil
		err := step.run(ctx, e)
		got = append(got, fmt.Sprintf("%s -> %s -> %s", step.name, errorSummary(err), strings.Join(rec.calls, " ")))
	}
	if len(got) != len(compatCallSequence) {
		t.Fatalf("scenario has %d steps, golden has %d", len(got), len(compatCallSequence))
	}
	for i := range got {
		if got[i] != compatCallSequence[i] {
			t.Errorf("step %d changed\ngot:  %s\nwant: %s", i, got[i], compatCallSequence[i])
		}
	}
}

// TestCompatEngineWithoutSchemas mirrors the globus transfer test, which builds engines
// with only a Store and a Role.
func TestCompatEngineWithoutSchemas(t *testing.T) {
	ctx := context.Background()
	source := newFakeStore()
	for _, item := range compatJSONItems() {
		source.items[item.ID] = item
	}
	export, err := New(Deps{Store: source, Role: DeploymentRoleCanonical}).ExportContent(ctx)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if export.Checksum != compatChecksum {
		t.Fatalf("export checksum = %s, want %s", export.Checksum, compatChecksum)
	}
	destination := newFakeStore()
	res, err := New(Deps{Store: destination, Role: DeploymentRoleSandbox}).ReplaceContent(ctx, *export)
	if err != nil {
		t.Fatalf("replace: %v", err)
	}
	if res.ItemCount != len(compatJSONItems()) || len(destination.items) != res.ItemCount {
		t.Fatalf("replace installed %d items (result %d)", len(destination.items), res.ItemCount)
	}
}
