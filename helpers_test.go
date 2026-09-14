package contentstore

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
	"testing"
)

// testType is a representative content type used across the core tests. It mirrors
// the shape of a real localized content type (the SEO Override) without the core
// depending on any project's concrete schema.
const testType ContentType = "seoOverride"

var testLocales = LocaleConfig{Default: "de", Supported: []string{"de", "fr"}}

// testRegistry builds a registry with a representative schema: localized symbol and
// rich-text fields plus a non-localized, required, read-only generated JSON field.
func testRegistry() *Registry {
	r := NewRegistry()
	r.Register(NewSchema(testType, "1", []FieldDefinition{
		{ID: "metaTitle", Type: FieldTypeSymbol, Localized: true},
		{ID: "metaDescription", Type: FieldTypeSymbol, Localized: true},
		{ID: "ranking", Type: FieldTypeInteger, Localized: false},
		{ID: "seoText", Type: FieldTypeRichText, Localized: true},
		{ID: "filterSelection", Type: FieldTypeJSON, Localized: false, Required: true, ReadOnly: true},
	}))
	return r
}

// testNewID returns a deterministic, unique ID generator for tests.
func testNewID() func() string {
	n := 0
	return func() string {
		n++
		return fmt.Sprintf("id-%d", n)
	}
}

func newTestEngine(store Store) *Engine {
	return New(Deps{Store: store, Schemas: testRegistry(), Locales: testLocales, NewID: testNewID()})
}

// wantKind asserts err is a *Error with the given Kind.
func wantKind(t *testing.T, err error, kind Kind) {
	t.Helper()
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("expected *Error, got %T (%v)", err, err)
	}
	if e.Kind != kind {
		t.Fatalf("expected error kind %d, got %d (%v)", kind, e.Kind, err)
	}
}

// mkFields builds ContentFields from a plain nested map for tests.
func mkFields(m map[string]map[string]any) ContentFields {
	out := ContentFields{}
	for field, byLocale := range m {
		lv := LocalizedFieldValues{}
		maps.Copy(lv, byLocale)
		out[field] = lv
	}
	return out
}

func validFields() ContentFields {
	return mkFields(map[string]map[string]any{
		"metaTitle":       {"de": "Cocktailkleider"},
		"filterSelection": {"de": map[string]any{"brand": "nike"}},
	})
}

// fakeStore is an in-memory Store for engine unit tests.
type fakeStore struct {
	mu    sync.Mutex
	items map[string]*ContentItem
}

func newFakeStore() *fakeStore { return &fakeStore{items: map[string]*ContentItem{}} }

func (f *fakeStore) GetByID(_ context.Context, id string) (*ContentItem, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if it, ok := f.items[id]; ok {
		return cloneContentItem(it), nil
	}
	return nil, ErrNotFound
}

func (f *fakeStore) GetByKey(_ context.Context, key string) (*ContentItem, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, it := range f.items {
		if it.Key == key {
			return cloneContentItem(it), nil
		}
	}
	return nil, ErrNotFound
}

func (f *fakeStore) Upsert(_ context.Context, item *ContentItem, expectedDraftRevision *int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	existing := f.items[item.ID]
	if expectedDraftRevision != nil {
		switch {
		case *expectedDraftRevision == 0:
			if existing != nil || f.hasKey(item.Key) {
				return ErrConflict
			}
		case existing == nil || existing.Draft == nil || existing.Draft.Revision != *expectedDraftRevision:
			return ErrConflict
		}
	}
	f.items[item.ID] = cloneContentItem(item)
	return nil
}

func (f *fakeStore) hasKey(key string) bool {
	if key == "" {
		return false
	}
	for _, item := range f.items {
		if item.Key == key {
			return true
		}
	}
	return false
}

// SetField mirrors Persistor.SetField for unit tests: it resolves the target (key upserts,
// id must exist), merges the single field/locale, and bumps the draft revision. The
// in-memory merge is a read-modify-write, which is fine single-threaded; the production
// persistor does this atomically in MongoDB.
func (f *fakeStore) SetField(_ context.Context, in FieldUpdate) (*ContentItem, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var item *ContentItem
	switch {
	case in.Match.Key != "":
		for _, it := range f.items {
			if it.Key == in.Match.Key {
				item = it
			}
		}
	case in.Match.ID != "":
		item = f.items[in.Match.ID]
		if item == nil {
			return nil, ErrNotFound
		}
	}
	if item == nil { // create, keyed by key ($setOnInsert fields)
		item = &ContentItem{ID: in.NewID, Key: in.Match.Key, OwnerRef: in.OwnerRef, CreatedAt: in.Now}
		f.items[item.ID] = item
	}
	revision := int64(1)
	createdAt := in.Now
	var base ContentFields
	if item.Draft != nil {
		revision = item.Draft.Revision + 1
		createdAt = item.Draft.CreatedAt
		base = item.Draft.Fields
	}
	item.Type = in.Type
	item.Draft = &ContentSnapshot{
		Revision:      revision,
		SchemaVersion: in.SchemaVersion,
		SchemaHash:    in.SchemaHash,
		Fields:        mergeFieldValue(base, in.FieldID, in.Locale, in.Value),
		CreatedAt:     createdAt,
		UpdatedAt:     in.Now,
	}
	item.UpdatedAt = in.Now
	return cloneContentItem(item), nil
}

// mergeFieldValue returns a deep copy of existing with fieldID/locale set to value. A nil
// value clears that locale (and drops the field entirely when it has no locales left).
func mergeFieldValue(existing ContentFields, fieldID, locale string, value FieldValue) ContentFields {
	out := ContentFields{}
	for f, byLocale := range existing {
		cp := LocalizedFieldValues{}
		maps.Copy(cp, byLocale)
		out[f] = cp
	}
	if value == nil {
		if lv := out[fieldID]; lv != nil {
			delete(lv, locale)
			if len(lv) == 0 {
				delete(out, fieldID)
			}
		}
		return out
	}
	if out[fieldID] == nil {
		out[fieldID] = LocalizedFieldValues{}
	}
	out[fieldID][locale] = value
	return out
}

func (f *fakeStore) Delete(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.items, id)
	return nil
}

func (f *fakeStore) FindPublished(_ context.Context, types []ContentType, ownerRef *OwnerRef) ([]*ContentItem, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*ContentItem
	for _, it := range f.items {
		if it.Published == nil || it.ArchivedAt != nil {
			continue
		}
		if len(types) > 0 && !containsType(types, it.Type) {
			continue
		}
		if ownerRef != nil && (it.OwnerRef == nil || *it.OwnerRef != *ownerRef) {
			continue
		}
		out = append(out, cloneContentItem(it))
	}
	return out, nil
}

func (f *fakeStore) FindAll(_ context.Context, types []ContentType, ownerRef *OwnerRef) ([]*ContentItem, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*ContentItem
	for _, it := range f.items {
		if it.ArchivedAt != nil {
			continue
		}
		if len(types) > 0 && !containsType(types, it.Type) {
			continue
		}
		if ownerRef != nil && (it.OwnerRef == nil || *it.OwnerRef != *ownerRef) {
			continue
		}
		out = append(out, cloneContentItem(it))
	}
	return out, nil
}

func (f *fakeStore) FindArchived(_ context.Context, types []ContentType) ([]*ContentItem, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*ContentItem
	for _, it := range f.items {
		if it.ArchivedAt == nil {
			continue
		}
		if len(types) > 0 && !containsType(types, it.Type) {
			continue
		}
		out = append(out, cloneContentItem(it))
	}
	return out, nil
}

func (f *fakeStore) ExportAll(_ context.Context) ([]*ContentItem, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*ContentItem, 0, len(f.items))
	for _, item := range f.items {
		out = append(out, cloneContentItem(item))
	}
	return out, nil
}

func (f *fakeStore) ReplaceAll(_ context.Context, items []*ContentItem) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	replacement := make(map[string]*ContentItem, len(items))
	for _, item := range items {
		replacement[item.ID] = cloneContentItem(item)
	}
	f.items = replacement
	return nil
}

func cloneContentItem(item *ContentItem) *ContentItem {
	if item == nil {
		return nil
	}
	clone := *item
	if item.Draft != nil {
		draft := *item.Draft
		clone.Draft = &draft
	}
	if item.Published != nil {
		published := *item.Published
		clone.Published = &published
	}
	if item.OwnerRef != nil {
		owner := *item.OwnerRef
		clone.OwnerRef = &owner
	}
	if item.ArchivedAt != nil {
		archivedAt := *item.ArchivedAt
		clone.ArchivedAt = &archivedAt
	}
	return &clone
}

func containsType(types []ContentType, t ContentType) bool {
	return slices.Contains(types, t)
}

// recordingNotifier captures emitted ContentChange events.
type recordingNotifier struct {
	changes []ContentChange
}

func (n *recordingNotifier) record(c ContentChange) { n.changes = append(n.changes, c) }

// seedPublishable creates a draft with the required filterSelection so it can be published,
// without adding setup writes to notifier assertions in the calling test.
func seedPublishable(t *testing.T, e *Engine, key, ownerID string) {
	t.Helper()
	notify := e.notify
	e.notify = nil
	defer func() { e.notify = notify }()
	if _, err := e.SetFieldValue(context.Background(), SetFieldValueInput{
		Match:    ContentItemMatch{Key: key},
		Type:     testType,
		OwnerRef: &OwnerRef{Source: "contentful", Type: "shopCategory", ID: ownerID},
		FieldID:  "filterSelection", Locale: "de", Value: map[string]any{"brand": "puma"},
	}); err != nil {
		t.Fatalf("seed draft: %v", err)
	}
}
