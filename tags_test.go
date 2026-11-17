package contentstore

import (
	"context"
	"encoding/hex"
	"maps"
	"math"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

const articleType ContentType = "article"

// tagRegistry adds a tagged type to the tree registry: bare tags, weighted topics in
// [0, 10] and a closed audience namespace, at most 5 tags per item. Tree documents take
// bare tags and weighted topics.
func tagRegistry() *Registry {
	r := treeRegistry()
	article := NewSchema(articleType, "1", []FieldDefinition{{ID: "title", Type: FieldTypeSymbol}})
	article.Tags = &TagPolicy{MaxTags: 5, Namespaces: []TagNamespace{
		{Name: ""},
		{Name: "topic", Weighted: true, MinWeight: new(0.0), MaxWeight: new(10.0)},
		{Name: "audience", Values: []string{"developers", "editors"}},
	}}
	r.Register(article)
	// Tree documents take tags too, so rankings can be checked against folder visibility.
	doc, _ := r.schemaFor(documentType)
	doc.Tags = &TagPolicy{Namespaces: []TagNamespace{{Name: ""}, {Name: "topic", Weighted: true}}}
	r.Register(doc)
	return r
}

func newTagEngine(store Store) *Engine {
	return New(Deps{Store: store, Schemas: tagRegistry(), Locales: testLocales, NewID: testNewID()})
}

func TestEscapeTagKey(t *testing.T) {
	tests := map[string]string{
		"topic:mongodb":  "topic:mongodb",
		"topic:node.js":  "topic:node%2Ejs",
		"$money":         "%24money",
		"100%":           "100%25",
		"2024":           "%32024",
		"topic:2024":     "topic:2024",
		"v1.2.3":         "v1%2E2%2E3",
		"ünïcödé":        "ünïcödé",
		"a%2Eb":          "a%252Eb",
		"0":              "%30",
		"12abc":          "12abc",
		"audience:a b c": "audience:a b c",
	}
	for id, want := range tests {
		got := escapeTagKey(id)
		if got != want {
			t.Errorf("escapeTagKey(%q) = %q, want %q", id, got, want)
		}
		if strings.ContainsAny(got, ".$") {
			t.Errorf("escaped key %q still contains a path or operator character", got)
		}
		if back, err := unescapeTagKey(got); err != nil || back != id {
			t.Errorf("unescapeTagKey(%q) = %q, %v; want %q", got, back, err, id)
		}
	}
}

func TestTagSetBSON(t *testing.T) {
	tags := TagSet{"topic:node.js": 2.5, "2024": 1, "$x": 0.5}
	wantDoc := bson.D{{Key: "%24x", Value: 0.5}, {Key: "%32024", Value: 1.0}, {Key: "topic:node%2Ejs", Value: 2.5}}
	want, err := bson.Marshal(wantDoc)
	if err != nil {
		t.Fatal(err)
	}

	// Addressable (struct field) and non-addressable (bson.D value) encodings must both
	// go through the escaping codec.
	snapshot, err := bson.Marshal(&ContentSnapshot{Revision: 1, Tags: tags})
	if err != nil {
		t.Fatal(err)
	}
	inDoc, err := bson.Marshal(bson.D{{Key: "tags", Value: tags}})
	if err != nil {
		t.Fatal(err)
	}
	for name, encoded := range map[string][]byte{"struct field": snapshot, "bson.D value": inDoc} {
		raw, err := bson.Raw(encoded).LookupErr("tags")
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := hex.EncodeToString(raw.Value); got != hex.EncodeToString(want) {
			t.Fatalf("%s: tags encoded as %v, want %v", name, raw, bson.Raw(want))
		}
	}

	var decoded ContentSnapshot
	if err := bson.Unmarshal(snapshot, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !maps.Equal(decoded.Tags, tags) {
		t.Fatalf("round trip = %v, want %v", decoded.Tags, tags)
	}

	// Integer weights decode; empty and missing sets stay empty; other types fail.
	ints, _ := bson.Marshal(bson.D{{Key: "a", Value: int32(2)}, {Key: "b", Value: int64(3)}})
	var fromInts TagSet
	if err := fromInts.UnmarshalBSON(ints); err != nil || fromInts["a"] != 2 || fromInts["b"] != 3 {
		t.Fatalf("integer weights = %v, %v", fromInts, err)
	}
	previous := TagSet{"stale": 1}
	if err := previous.UnmarshalBSON(nil); err != nil || previous != nil {
		t.Fatalf("null must replace the set with an empty one: %v, %v", previous, err)
	}
	bad, _ := bson.Marshal(bson.D{{Key: "a", Value: "heavy"}})
	if err := new(TagSet).UnmarshalBSON(bad); err == nil {
		t.Fatal("a non-numeric weight must fail")
	}
	empty, err := bson.Marshal(&ContentSnapshot{Revision: 1, Tags: TagSet{}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bson.Raw(empty).LookupErr("tags"); err == nil {
		t.Fatal("an empty tag set must be omitted")
	}
}

func TestValidateTagID(t *testing.T) {
	valid := []string{"mongodb", "topic:mongodb", "topic:node.js", "a-b_c:x", "topic:a:b", "audience:a b", "2024", strings.Repeat("v", 200)}
	invalid := []string{"", "topic:", ":x", "Topic:x", "-x:y", "topic: x", "topic:x ", "x\ny", "\xff", strings.Repeat("v", 201), strings.Repeat("n", 65) + ":x"}
	for _, id := range valid {
		if err := validateTagID(id); err != nil {
			t.Errorf("validateTagID(%q) = %v, want nil", id, err)
		}
	}
	for _, id := range invalid {
		if err := validateTagID(id); err == nil {
			t.Errorf("validateTagID(%q) accepted an invalid ID", id)
		}
	}
}

func TestRegisterRejectsInvalidTagPolicy(t *testing.T) {
	tests := map[string]TagPolicy{
		"negative max":                   {MaxTags: -1},
		"max above limit":                {MaxTags: maxTagsLimit + 1},
		"invalid namespace":              {Namespaces: []TagNamespace{{Name: "Topic"}}},
		"duplicate namespace":            {Namespaces: []TagNamespace{{Name: "topic"}, {Name: "topic"}}},
		"invalid value":                  {Namespaces: []TagNamespace{{Name: "topic", Values: []string{" x"}}}},
		"duplicate value":                {Namespaces: []TagNamespace{{Name: "topic", Values: []string{"x", "x"}}}},
		"colon in a default value":       {Namespaces: []TagNamespace{{Name: "", Values: []string{"a:b"}}}},
		"bounds on unweighted namespace": {Namespaces: []TagNamespace{{Name: "topic", MinWeight: new(0.0)}}},
		"infinite bound":                 {Namespaces: []TagNamespace{{Name: "topic", Weighted: true, MaxWeight: new(math.Inf(1))}}},
		"min above max":                  {Namespaces: []TagNamespace{{Name: "topic", Weighted: true, MinWeight: new(2.0), MaxWeight: new(1.0)}}},
	}
	for name, policy := range tests {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("Register accepted an invalid tag policy")
				}
			}()
			schema := NewSchema("x", "1", nil)
			schema.Tags = &policy
			NewRegistry().Register(schema)
		})
	}
}

func TestTagPolicyCheckTag(t *testing.T) {
	policy := tagRegistry().schemas[articleType].Tags
	tests := []struct {
		id      string
		weight  *float64
		want    float64
		wantErr bool
	}{
		{"mongodb", nil, 1, false},
		{"topic:databases", new(7.5), 7.5, false},
		{"topic:databases", nil, 1, false},
		{"audience:developers", nil, 1, false},
		{"audience:managers", nil, 0, true},     // closed namespace
		{"audience:editors", new(2.0), 0, true}, // unweighted namespace
		{"topic:databases", new(11.0), 0, true}, // above maxWeight
		{"topic:databases", new(-1.0), 0, true}, // below minWeight
		{"topic:databases", new(math.NaN()), 0, true},
		{"region:emea", nil, 0, true}, // undeclared namespace
		{"topic:", nil, 0, true},      // syntax
	}
	for _, tt := range tests {
		got, err := policy.checkTag(tt.id, tt.weight)
		if (err != nil) != tt.wantErr || (!tt.wantErr && got != tt.want) {
			t.Errorf("checkTag(%q, %v) = %v, %v; want %v, err=%v", tt.id, tt.weight, got, err, tt.want, tt.wantErr)
		}
	}
	bareless := &TagPolicy{Namespaces: []TagNamespace{{Name: "topic"}}}
	if _, err := bareless.checkTag("mongodb", nil); err == nil {
		t.Fatal("bare tags need a default namespace in the policy")
	}
	ranged := &TagPolicy{Namespaces: []TagNamespace{{Name: "score", Weighted: true, MinWeight: new(2.0)}}}
	if _, err := ranged.checkTag("score:x", nil); err == nil {
		t.Fatal("the default weight 1 must respect the namespace bounds")
	}
}

// seedTags writes draft tags straight into the fake store.
func seedTags(t *testing.T, store *fakeStore, id string, tags TagSet) {
	t.Helper()
	store.mu.Lock()
	defer store.mu.Unlock()
	store.items[id].Draft.Tags = tags
}

func TestTagsTravelWithSnapshots(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	e := newTagEngine(store)
	item, err := e.SaveDraft(ctx, SaveDraftInput{Type: articleType, Fields: titleFields("v1")})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	match := ContentItemMatch{ID: item.ID}
	seedTags(t, store, item.ID, TagSet{"topic:databases": 7, "mongodb": 1})

	if _, err := e.SaveDraft(ctx, SaveDraftInput{Match: match, Fields: titleFields("v2")}); err != nil {
		t.Fatalf("save: %v", err)
	}
	if _, err := e.SetFieldValue(ctx, SetFieldValueInput{Match: match, FieldID: "title", Locale: "de", Value: "v3"}); err != nil {
		t.Fatalf("set field: %v", err)
	}
	if got := getItem(t, e, item.ID).Draft.Tags; len(got) != 2 || got["topic:databases"] != 7 {
		t.Fatalf("draft writes must keep tags, got %v", got)
	}

	if _, err := e.Publish(ctx, match); err != nil {
		t.Fatalf("publish: %v", err)
	}
	seedTags(t, store, item.ID, TagSet{"mongodb": 1})
	published, err := e.GetSnapshot(ctx, SnapshotViewPublished, SnapshotInput{Types: []ContentType{articleType}})
	if err != nil || len(published.Items) != 1 || len(published.Items[0].Tags) != 2 {
		t.Fatalf("published view must show the published tags: %+v, %v", published, err)
	}
	preview, err := e.GetSnapshot(ctx, SnapshotViewPreview, SnapshotInput{Types: []ContentType{articleType}})
	if err != nil || len(preview.Items) != 1 || len(preview.Items[0].Tags) != 1 {
		t.Fatalf("preview must show the draft tags: %+v, %v", preview, err)
	}

	if _, err := e.Archive(ctx, match); err != nil {
		t.Fatalf("archive: %v", err)
	}
	archived, err := e.GetArchivedSnapshot(ctx, ArchivedSnapshotInput{})
	if err != nil || len(archived.Items) != 1 || archived.Items[0].Tags["mongodb"] != 1 {
		t.Fatalf("archived snapshot must carry the draft tags: %+v, %v", archived, err)
	}
}

// TestPublishNeverRevalidatesTags: tags are validated when written, so a narrowed policy
// (closed values, namespaces or a lowered MaxTags) never blocks publishing existing content.
func TestPublishNeverRevalidatesTags(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	e := newTagEngine(store)
	item, err := e.SaveDraft(ctx, SaveDraftInput{Type: articleType, Fields: titleFields("v1")})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	seedTags(t, store, item.ID, TagSet{"audience:retired": 1, "region:emea": 1, "a": 1, "b": 1, "c": 1, "d": 1})
	if _, err := e.Publish(ctx, ContentItemMatch{ID: item.ID}); err != nil {
		t.Fatalf("narrowed policies must not block publish: %v", err)
	}
}
