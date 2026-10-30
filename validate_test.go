package contentstore

import (
	"context"
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

// sizedJSON returns a JSON object whose serialized form is exactly n bytes.
func sizedJSON(n int) map[string]any {
	return map[string]any{"t": strings.Repeat("a", n-len(`{"t":""}`))}
}

func TestFieldLimitDefaultsUnchanged(t *testing.T) {
	tests := []struct {
		fieldType FieldType
		atLimit   FieldValue
		overLimit FieldValue
	}{
		{FieldTypeSymbol, strings.Repeat("a", 256), strings.Repeat("a", 257)},
		{FieldTypeText, strings.Repeat("a", 50_000), strings.Repeat("a", 50_001)},
		{FieldTypeRichText, sizedJSON(200_000), sizedJSON(200_001)},
		{FieldTypeJSON, sizedJSON(200_000), sizedJSON(200_001)},
	}
	for _, tt := range tests {
		t.Run(string(tt.fieldType), func(t *testing.T) {
			fd := FieldDefinition{ID: "f", Type: tt.fieldType}
			if err := validateValue(fd, tt.atLimit); err != nil {
				t.Fatalf("value at the default limit rejected: %v", err)
			}
			if err := validateValue(fd, tt.overLimit); err == nil {
				t.Fatal("value over the default limit accepted")
			}
		})
	}
}

func TestFieldMaxLengthOverridesDefault(t *testing.T) {
	tests := []struct {
		name      string
		fd        FieldDefinition
		atLimit   FieldValue
		overLimit FieldValue
	}{
		{"raised text", FieldDefinition{Type: FieldTypeText, MaxLength: new(100_000)}, strings.Repeat("a", 100_000), strings.Repeat("a", 100_001)},
		{"lowered symbol", FieldDefinition{Type: FieldTypeSymbol, MaxLength: new(10)}, strings.Repeat("a", 10), strings.Repeat("a", 11)},
		{"raised richtext", FieldDefinition{Type: FieldTypeRichText, MaxLength: new(300_000)}, sizedJSON(300_000), sizedJSON(300_001)},
		{"lowered json", FieldDefinition{Type: FieldTypeJSON, MaxLength: new(100)}, sizedJSON(100), sizedJSON(101)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := validateValue(tt.fd, tt.atLimit); err != nil {
				t.Fatalf("value at MaxLength rejected: %v", err)
			}
			if err := validateValue(tt.fd, tt.overLimit); err == nil {
				t.Fatal("value over MaxLength accepted")
			}
		})
	}
}

func TestRegisterRejectsInvalidMaxLength(t *testing.T) {
	tests := []struct {
		name string
		fd   FieldDefinition
	}{
		{"zero", FieldDefinition{ID: "f", Type: FieldTypeText, MaxLength: new(0)}},
		{"negative", FieldDefinition{ID: "f", Type: FieldTypeText, MaxLength: new(-1)}},
		{"above ceiling", FieldDefinition{ID: "f", Type: FieldTypeText, MaxLength: new(MaxFieldLength + 1)}},
		{"integer field", FieldDefinition{ID: "f", Type: FieldTypeInteger, MaxLength: new(10)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("Register accepted an invalid MaxLength")
				}
			}()
			NewRegistry().Register(NewSchema("doc", "1", []FieldDefinition{tt.fd}))
		})
	}
	// The ceiling itself is valid.
	NewRegistry().Register(NewSchema("doc", "1", []FieldDefinition{{ID: "f", Type: FieldTypeText, MaxLength: new(MaxFieldLength)}}))
}

func TestSchemaHashCoversMaxLengthOnlyWhenSet(t *testing.T) {
	plain := NewSchema("doc", "1", []FieldDefinition{{ID: "body", Type: FieldTypeText}})
	limited := NewSchema("doc", "1", []FieldDefinition{{ID: "body", Type: FieldTypeText, MaxLength: new(100_000)}})
	other := NewSchema("doc", "1", []FieldDefinition{{ID: "body", Type: FieldTypeText, MaxLength: new(200_000)}})
	if plain.Hash == limited.Hash || limited.Hash == other.Hash {
		t.Fatalf("MaxLength must change the hash when set: plain=%s limited=%s other=%s", plain.Hash, limited.Hash, other.Hash)
	}
}

// largeDocEngine registers a type whose four non-localized text fields may each hold the
// maximum value, so together they can exceed the draft size guard.
func largeDocEngine() *Engine {
	reg := testRegistry()
	fields := make([]FieldDefinition, 0, 4)
	for _, id := range []FieldID{"a", "b", "c", "d"} {
		fields = append(fields, FieldDefinition{ID: id, Type: FieldTypeText, MaxLength: new(MaxFieldLength)})
	}
	reg.Register(NewSchema("largeDoc", "1", fields))
	return New(Deps{Store: newFakeStore(), Schemas: reg, Locales: testLocales, NewID: testNewID()})
}

func TestDraftSizeGuard(t *testing.T) {
	ctx := context.Background()
	big := strings.Repeat("a", 1_900_000)
	e := largeDocEngine()

	tooLarge := mkFields(map[string]map[string]any{"a": {"de": big}, "b": {"de": big}, "c": {"de": big}, "d": {"de": big}})
	_, err := e.SaveDraft(ctx, SaveDraftInput{Match: ContentItemMatch{Key: "doc"}, Type: "largeDoc", Fields: tooLarge})
	wantKind(t, err, KindBadRequest)

	fits := mkFields(map[string]map[string]any{"a": {"de": big}, "b": {"de": big}, "c": {"de": big}})
	if _, err := e.SaveDraft(ctx, SaveDraftInput{Match: ContentItemMatch{Key: "doc"}, Type: "largeDoc", Fields: fits}); err != nil {
		t.Fatalf("draft below the guard rejected: %v", err)
	}

	// SetFieldValue checks the merged draft, not the single value.
	_, err = e.SetFieldValue(ctx, SetFieldValueInput{Match: ContentItemMatch{Key: "doc"}, FieldID: "d", Locale: "de", Value: big})
	wantKind(t, err, KindBadRequest)
	item, err := e.GetItem(ctx, ContentItemMatch{Key: "doc"})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if _, ok := item.Draft.Fields["d"]; ok || item.Draft.Revision != 1 {
		t.Fatalf("rejected SetFieldValue must not write: revision=%d fields=%v", item.Draft.Revision, len(item.Draft.Fields))
	}

	// Replacing a value with a smaller one is always allowed.
	if _, err := e.SetFieldValue(ctx, SetFieldValueInput{Match: ContentItemMatch{Key: "doc"}, FieldID: "a", Locale: "de", Value: "small"}); err != nil {
		t.Fatalf("shrinking a value rejected: %v", err)
	}
}

func TestPublishRechecksDraftSize(t *testing.T) {
	ctx := context.Background()
	e := largeDocEngine()
	store := e.store.(*fakeStore)
	big := strings.Repeat("a", 1_900_000)
	// A draft that outgrew the guard through concurrent SetFieldValue calls.
	store.items["doc"] = &ContentItem{ID: "doc", Type: "largeDoc", Draft: &ContentSnapshot{
		Revision: 4,
		Fields:   mkFields(map[string]map[string]any{"a": {"de": big}, "b": {"de": big}, "c": {"de": big}, "d": {"de": big}}),
	}}
	_, err := e.Publish(ctx, ContentItemMatch{ID: "doc"})
	wantKind(t, err, KindBadRequest)
	if store.items["doc"].Published != nil {
		t.Fatal("an oversized draft must not be published")
	}
}
