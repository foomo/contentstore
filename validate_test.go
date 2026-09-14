package contentstore

import (
	"math"
	"strings"
	"testing"
)

func TestValidateDraft(t *testing.T) {
	e := newTestEngine(newFakeStore())
	tests := []struct {
		name    string
		fields  ContentFields
		wantErr bool
	}{
		{"valid symbol + integer + richtext + json", mkFields(map[string]map[string]any{
			"metaTitle":       {"de": "Titel", "fr": "Titre"},
			"ranking":         {"de": float64(3)},
			"seoText":         {"de": map[string]any{"nodeType": "document"}},
			"filterSelection": {"de": map[string]any{"brand": "nike"}},
		}), false},
		{"nil value clears locale", mkFields(map[string]map[string]any{"metaTitle": {"de": nil}}), false},
		{"unknown field", mkFields(map[string]map[string]any{"nope": {"de": "x"}}), true},
		{"symbol not a string", mkFields(map[string]map[string]any{"metaTitle": {"de": 42}}), true},
		{"integer as Go int", mkFields(map[string]map[string]any{"ranking": {"de": 3}}), false},
		{"integer rejects fraction", mkFields(map[string]map[string]any{"ranking": {"de": 3.5}}), true},
		{"integer rejects string", mkFields(map[string]map[string]any{"ranking": {"de": "3"}}), true},
		{"integer rejects uint overflow", mkFields(map[string]map[string]any{"ranking": {"de": uint64(math.MaxUint64)}}), true},
		{"symbol too long", mkFields(map[string]map[string]any{"metaTitle": {"de": strings.Repeat("a", 257)}}), true},
		{"richtext not an object", mkFields(map[string]map[string]any{"seoText": {"de": "plain"}}), true},
		{"unsupported locale", mkFields(map[string]map[string]any{"metaTitle": {"it": "x"}}), true},
		{"non-localized field in non-default locale", mkFields(map[string]map[string]any{"filterSelection": {"fr": map[string]any{"brand": "nike"}}}), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := e.validateDraft(testType, tt.fields)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateDraft() err=%v, wantErr=%v", err, tt.wantErr)
			}
		})
	}
}

func TestValidateDraftUnknownType(t *testing.T) {
	e := newTestEngine(newFakeStore())
	if err := e.validateDraft("nope", nil); err == nil {
		t.Fatal("expected error for unknown content type")
	}
}

func TestValidatePublish(t *testing.T) {
	e := newTestEngine(newFakeStore())
	// filterSelection is required for publish.
	missing := mkFields(map[string]map[string]any{"metaTitle": {"de": "Titel"}})
	if err := e.validatePublish(testType, missing); err == nil {
		t.Fatal("expected error for missing filterSelection")
	} else {
		wantKind(t, err, KindNotAcceptable)
	}
	ok := mkFields(map[string]map[string]any{"filterSelection": {"de": map[string]any{"brand": "nike"}}})
	if err := e.validatePublish(testType, ok); err != nil {
		t.Fatalf("expected publish to be valid, got %v", err)
	}
}

func TestSchemaRegistry(t *testing.T) {
	r := testRegistry()
	s, ok := r.schemaFor(testType)
	if !ok {
		t.Fatal("expected registered schema")
	}
	if s.Hash == "" || s.Version == "" {
		t.Fatalf("expected version+hash, got %#v", s)
	}
	if fs, ok := s.field("filterSelection"); !ok || !fs.ReadOnly || !fs.Required || fs.Localized {
		t.Fatalf("filterSelection must be required, read-only, non-localized: %#v", fs)
	}
	if len(r.all()) != 1 {
		t.Fatalf("expected exactly one registered schema")
	}
}
