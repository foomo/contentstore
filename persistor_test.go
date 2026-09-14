package contentstore

import (
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// operatorDoc returns the bson.D value of a top-level update operator (e.g. "$set").
func operatorDoc(t *testing.T, update bson.D, op string) bson.D {
	t.Helper()
	for _, e := range update {
		if e.Key == op {
			d, ok := e.Value.(bson.D)
			if !ok {
				t.Fatalf("operator %q is not a bson.D: %#v", op, e.Value)
			}
			return d
		}
	}
	return nil
}

func lookup(d bson.D, key string) (any, bool) {
	for _, e := range d {
		if e.Key == key {
			return e.Value, true
		}
	}
	return nil, false
}

// TestFieldUpdateDocumentTargetsSingleFieldPath locks in the property that fixes the
// concurrent-write corruption: the update touches exactly one field/locale path and never
// rewrites draft.fields wholesale, so simultaneous writes to other fields survive.
func TestFieldUpdateDocumentTargetsSingleFieldPath(t *testing.T) {
	doc := fieldUpdateDocument(FieldUpdate{
		Type:          testType,
		OwnerRef:      &OwnerRef{Source: "contentful", Type: "shopCategory", ID: "cat-9"},
		FieldID:       "metaTitle",
		Locale:        "de",
		Value:         "Puma",
		SchemaVersion: "1",
		SchemaHash:    "h",
		Now:           time.Unix(0, 0).UTC(),
		NewID:         "id-1",
	})

	set := operatorDoc(t, doc, "$set")
	if v, ok := lookup(set, "draft.fields.metaTitle.de"); !ok || v != "Puma" {
		t.Fatalf("expected targeted $set on draft.fields.metaTitle.de, got %#v", set)
	}
	// Must NOT rewrite the whole field set — that is exactly what clobbers concurrent writers.
	if _, ok := lookup(set, "draft.fields"); ok {
		t.Fatalf("$set must not target draft.fields wholesale: %#v", set)
	}

	inc := operatorDoc(t, doc, "$inc")
	if v, ok := lookup(inc, "draft.revision"); !ok || v.(int64) != 1 {
		t.Fatalf("expected $inc draft.revision by 1, got %#v", inc)
	}

	onInsert := operatorDoc(t, doc, "$setOnInsert")
	if v, ok := lookup(onInsert, "_id"); !ok || v != "id-1" {
		t.Fatalf("expected $setOnInsert _id=id-1, got %#v", onInsert)
	}
	if _, ok := lookup(onInsert, "ownerRef"); !ok {
		t.Fatalf("expected owner set only on insert, got %#v", onInsert)
	}
	if _, ok := lookup(onInsert, "createdAt"); !ok {
		t.Fatalf("expected createdAt set only on insert, got %#v", onInsert)
	}
	// A create writes revision via $inc (absent field → 1), never as a fixed $set/$setOnInsert.
	if _, ok := lookup(onInsert, "draft.revision"); ok {
		t.Fatalf("revision must come from $inc, not $setOnInsert: %#v", onInsert)
	}
}

// TestFieldUpdateDocumentClearsLocaleWithUnset verifies a nil value clears just that
// locale via $unset (not a whole-field rewrite).
func TestFieldUpdateDocumentClearsLocaleWithUnset(t *testing.T) {
	doc := fieldUpdateDocument(FieldUpdate{
		Type: testType, FieldID: "metaTitle", Locale: "fr", Value: nil,
		Now: time.Unix(0, 0).UTC(), NewID: "id-1",
	})
	unset := operatorDoc(t, doc, "$unset")
	if _, ok := lookup(unset, "draft.fields.metaTitle.fr"); !ok {
		t.Fatalf("expected $unset on draft.fields.metaTitle.fr, got %#v", unset)
	}
	if set := operatorDoc(t, doc, "$set"); set != nil {
		if _, ok := lookup(set, "draft.fields.metaTitle.fr"); ok {
			t.Fatalf("a nil value must not also $set the path: %#v", set)
		}
	}
}

// TestFieldUpdateFilter verifies key matches upsert (create-or-update) while id-only
// matches require an existing document.
func TestFieldUpdateFilter(t *testing.T) {
	if f, upsert := fieldUpdateFilter(ContentItemMatch{Key: "k"}); !upsert {
		t.Fatalf("key match must upsert, got filter %#v", f)
	} else if v, ok := lookup(f, "key"); !ok || v != "k" {
		t.Fatalf("key filter wrong: %#v", f)
	}
	if f, upsert := fieldUpdateFilter(ContentItemMatch{ID: "i"}); upsert {
		t.Fatalf("id match must not upsert, got filter %#v", f)
	} else if v, ok := lookup(f, "_id"); !ok || v != "i" {
		t.Fatalf("id filter wrong: %#v", f)
	}
}

func TestDraftReplaceFilterIncludesExpectedRevision(t *testing.T) {
	expected := int64(7)
	filter := draftReplaceFilter("item-1", &expected)
	if id, ok := lookup(filter, "_id"); !ok || id != "item-1" {
		t.Fatalf("expected _id filter, got %#v", filter)
	}
	if revision, ok := lookup(filter, "draft.revision"); !ok || revision != expected {
		t.Fatalf("expected draft revision filter, got %#v", filter)
	}

	unconditional := draftReplaceFilter("item-1", nil)
	if _, ok := lookup(unconditional, "draft.revision"); ok {
		t.Fatalf("unconditional filter must not include a revision: %#v", unconditional)
	}
}
