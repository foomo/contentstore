package contentstore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strconv"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// ContentVersions is the version-history API of the engine. *Engine implements it. It is
// separate from ContentStore so existing implementations of that interface keep compiling.
//
// Every publish of an item whose type has a VersionPolicy records the published snapshot
// as a version, identified by its revision. ListVersions and GetVersion read them,
// CompareVersions diffs two revisions, and RestoreVersion writes one back as the draft.
type ContentVersions interface {
	ListVersions(ctx context.Context, input ListVersionsInput) (*VersionPage, error)
	GetVersion(ctx context.Context, match ContentItemMatch, revision int64) (*ContentVersion, error)
	CompareVersions(ctx context.Context, input CompareVersionsInput) (*VersionDiff, error)
	RestoreVersion(ctx context.Context, input RestoreVersionInput) (*RestoreVersionResult, error)
}

var _ ContentVersions = (*Engine)(nil)

type authorKey struct{}

// WithAuthor returns a context that attributes the versions published with it to author,
// e.g. an editor's user ID (see ContentVersion.PublishedBy). Scheduled publishes take the
// author of the context given to RunDueActions.
func WithAuthor(ctx context.Context, author string) context.Context {
	return context.WithValue(ctx, authorKey{}, author)
}

func authorFrom(ctx context.Context) string {
	author, _ := ctx.Value(authorKey{}).(string)
	return author
}

// requireVersions rejects version calls on an engine whose store keeps none.
func (e *Engine) requireVersions() error {
	if e.versions == nil {
		return internalErr(nil, "store does not keep versions (see WithVersions)")
	}
	return nil
}

// versionPolicy returns the version policy of type t (nil: its publishes are not recorded).
func (e *Engine) versionPolicy(t ContentType) *VersionPolicy {
	schema, ok := e.schemas.schemaFor(t)
	if !ok {
		return nil
	}
	return schema.Versions
}

// requireVersionsFor rejects publishing an item of type t that must be recorded when the
// store keeps no versions, before anything is written.
func (e *Engine) requireVersionsFor(t ContentType) error {
	if e.versionPolicy(t) != nil && e.versions == nil {
		return internalErr(nil, fmt.Sprintf("type %q keeps versions but the store does not (see WithVersions)", t))
	}
	return nil
}

// recordVersions records the published snapshot of a just-published item as a version,
// and prunes the item's oldest versions. It also records the published snapshot the
// publish replaced, when that is another revision: the item may have been published before
// its type kept versions, or a tag merge or deletion may have rewritten it since.
func (e *Engine) recordVersions(ctx context.Context, item *ContentItem, replaced *ContentSnapshot) error {
	policy := e.versionPolicy(item.Type)
	if policy == nil {
		return nil
	}
	var versions []*ContentVersion
	if replaced != nil && replaced.Revision != item.Published.Revision {
		versions = append(versions, &ContentVersion{
			ItemID: item.ID, Revision: replaced.Revision, PublishedAt: replaced.UpdatedAt, Snapshot: replaced,
		})
	}
	published := *item.Published
	versions = append(versions, &ContentVersion{
		ItemID: item.ID, Revision: published.Revision, PublishedAt: published.UpdatedAt,
		PublishedBy: authorFrom(ctx), Snapshot: &published,
	})
	for _, v := range versions {
		if err := e.versions.InsertVersion(ctx, v); err != nil {
			return versionWriteErr(err)
		}
	}
	if err := e.versions.PruneVersions(ctx, item.ID, policy.keep()); err != nil {
		return versionWriteErr(err)
	}
	return nil
}

func versionWriteErr(err error) *Error {
	return internalErr(err, "recording the published version failed: publishing again records it")
}

// dropVersions deletes the versions of a deleted item. A failure is ignored: the item is
// gone either way, and its versions can no longer be addressed.
func (e *Engine) dropVersions(ctx context.Context, itemID string) {
	if e.versions == nil {
		return
	}
	_ = e.versions.DeleteVersions(ctx, itemID)
}

// ListVersions returns one page of an item's versions, newest first, without their
// snapshots.
func (e *Engine) ListVersions(ctx context.Context, input ListVersionsInput) (*VersionPage, error) {
	limit := input.Limit
	switch {
	case limit == 0:
		limit = defaultChildrenLimit
	case limit < 0 || limit > maxChildrenLimit:
		return nil, badRequest(fmt.Sprintf("limit must be between 1 and %d", maxChildrenLimit))
	}
	before, err := decodeVersionCursor(input.Cursor)
	if err != nil {
		return nil, err
	}
	if err := e.requireVersions(); err != nil {
		return nil, err
	}
	item, err := e.loadByMatch(ctx, input.Match)
	if err != nil {
		return nil, err
	}
	found, err := e.versions.FindVersions(ctx, VersionQuery{ItemID: item.ID, Before: before, Limit: limit + 1})
	if err != nil {
		return nil, internalErr(err, "list versions failed")
	}
	page := &VersionPage{Items: make([]ContentVersion, 0, min(len(found), limit))}
	if len(found) > limit {
		found = found[:limit]
		page.NextCursor = strconv.FormatInt(found[limit-1].Revision, 10)
	}
	for _, v := range found {
		page.Items = append(page.Items, *v)
	}
	return page, nil
}

func decodeVersionCursor(cursor string) (int64, error) {
	if cursor == "" {
		return 0, nil
	}
	revision, err := strconv.ParseInt(cursor, 10, 64)
	if err != nil || revision <= 0 {
		return 0, badRequest("invalid cursor")
	}
	return revision, nil
}

// GetVersion returns an item's recorded version of the given revision.
func (e *Engine) GetVersion(ctx context.Context, match ContentItemMatch, revision int64) (*ContentVersion, error) {
	if revision <= 0 {
		return nil, badRequest("revision must be positive")
	}
	if err := e.requireVersions(); err != nil {
		return nil, err
	}
	item, err := e.loadByMatch(ctx, match)
	if err != nil {
		return nil, err
	}
	return e.version(ctx, item, revision)
}

func (e *Engine) version(ctx context.Context, item *ContentItem, revision int64) (*ContentVersion, error) {
	v, err := e.versions.GetVersion(ctx, item.ID, revision)
	if errors.Is(err, ErrNotFound) {
		return nil, notFound(fmt.Sprintf("item %q has no version %d", item.ID, revision))
	}
	if err != nil {
		return nil, internalErr(err, "get version failed")
	}
	return v, nil
}

// snapshotAt returns the snapshot of an item's revision: its draft (also for revision 0),
// its published snapshot, or a recorded version.
func (e *Engine) snapshotAt(ctx context.Context, item *ContentItem, revision int64) (*ContentSnapshot, error) {
	switch {
	case revision == 0 || (item.Draft != nil && item.Draft.Revision == revision):
		if item.Draft == nil {
			return nil, notAcceptable("item has no draft")
		}
		return item.Draft, nil
	case item.Published != nil && item.Published.Revision == revision:
		return item.Published, nil
	}
	v, err := e.version(ctx, item, revision)
	if err != nil {
		return nil, err
	}
	return v.Snapshot, nil
}

// CompareVersions lists the field values and tags that differ between two revisions of an
// item: recorded versions, its published or draft revision, or 0 for the current draft.
// Values are compared by their JSON encoding, so the same number stored as an integer or a
// float is equal.
func (e *Engine) CompareVersions(ctx context.Context, input CompareVersionsInput) (*VersionDiff, error) {
	if input.From < 0 || input.To < 0 {
		return nil, badRequest("revisions must not be negative")
	}
	if err := e.requireVersions(); err != nil {
		return nil, err
	}
	item, err := e.loadByMatch(ctx, input.Match)
	if err != nil {
		return nil, err
	}
	from, err := e.snapshotAt(ctx, item, input.From)
	if err != nil {
		return nil, err
	}
	to, err := e.snapshotAt(ctx, item, input.To)
	if err != nil {
		return nil, err
	}
	return &VersionDiff{
		From:   from.Revision,
		To:     to.Revision,
		Fields: diffFields(from.Fields, to.Fields),
		Tags:   diffTags(from.Tags, to.Tags),
	}, nil
}

// diffFields lists the field locales whose values differ, by field ID and locale. A nil
// value counts as no value.
func diffFields(from, to ContentFields) []FieldChange {
	changes := []FieldChange{}
	for _, fieldID := range slices.Sorted(maps.Keys(unionKeys(from, to))) {
		for _, locale := range slices.Sorted(maps.Keys(unionKeys(from[fieldID], to[fieldID]))) {
			a, b := from[fieldID][locale], to[fieldID][locale]
			if (a == nil && b == nil) || (a != nil && b != nil && equalValues(a, b)) {
				continue
			}
			changes = append(changes, FieldChange{FieldID: fieldID, Locale: locale, From: a, To: b})
		}
	}
	return changes
}

// diffTags lists the tags that were added, removed or re-weighted, by ID.
func diffTags(from, to TagSet) []TagChange {
	changes := []TagChange{}
	for _, id := range slices.Sorted(maps.Keys(unionKeys(from, to))) {
		a, inFrom := from[id]
		b, inTo := to[id]
		if inFrom && inTo && a == b {
			continue
		}
		change := TagChange{ID: id}
		if inFrom {
			change.From = new(a)
		}
		if inTo {
			change.To = new(b)
		}
		changes = append(changes, change)
	}
	return changes
}

func unionKeys[M ~map[string]V, V any](a, b M) map[string]struct{} {
	keys := make(map[string]struct{}, len(a)+len(b))
	for k := range a {
		keys[k] = struct{}{}
	}
	for k := range b {
		keys[k] = struct{}{}
	}
	return keys
}

// equalValues compares two field values by their JSON encoding, which ignores map order
// and the Go types of numbers and decoded documents.
func equalValues(a, b FieldValue) bool {
	x, errA := json.Marshal(a)
	y, errB := json.Marshal(b)
	if errA != nil || errB != nil {
		return reflect.DeepEqual(a, b)
	}
	return bytes.Equal(x, y)
}

// RestoreVersion writes a revision of an item (a recorded version, or its published
// revision) back as its draft: its fields and tags become a new draft revision, which is not
// published. Field values and tags that the type's current schema or tag policy no longer
// accepts are left out and reported; other values the schema rejects (e.g. too long since a
// limit was lowered) fail the restore. Placement is not versioned and stays as it is.
// Restoring the content the draft already has changes nothing. It emits OpSaveDraft.
func (e *Engine) RestoreVersion(ctx context.Context, input RestoreVersionInput) (*RestoreVersionResult, error) {
	if input.Revision <= 0 {
		return nil, badRequest("revision must be positive")
	}
	if input.ExpectedRevision != nil && *input.ExpectedRevision <= 0 {
		return nil, badRequest("expectedRevision must be positive")
	}
	if err := e.requireVersions(); err != nil {
		return nil, err
	}
	loaded, err := e.loadByMatch(ctx, input.Match)
	if err != nil {
		return nil, err
	}
	if err := rejectArchived(loaded); err != nil {
		return nil, err
	}
	snapshot, err := e.snapshotAt(ctx, loaded, input.Revision)
	if err != nil {
		return nil, err
	}
	schema, ok := e.schemas.schemaFor(loaded.Type)
	if !ok {
		return nil, badRequest(fmt.Sprintf("unknown content type %q", loaded.Type))
	}
	fields, droppedFields := e.restorableFields(schema, snapshot.Fields)
	tags, droppedTags, err := restorableTags(schema, snapshot.Tags)
	if err != nil {
		return nil, err
	}
	if err := e.validateDraft(loaded.Type, fields); err != nil {
		return nil, err
	}
	if e.guardsDraftSize(loaded.Type) {
		if err := validateDraftSize(fields); err != nil {
			return nil, err
		}
	}

	restore := func(_ context.Context, item *ContentItem) (bool, error) {
		if err := rejectArchived(item); err != nil {
			return false, err
		}
		if item.Draft == nil {
			return false, notAcceptable("item has no draft")
		}
		if input.ExpectedRevision != nil && item.Draft.Revision != *input.ExpectedRevision {
			return false, conflict("draft revision conflict")
		}
		if len(diffFields(item.Draft.Fields, fields)) == 0 && maps.Equal(item.Draft.Tags, tags) {
			return false, nil
		}
		now := time.Now().UTC()
		item.Draft = &ContentSnapshot{
			Revision:      item.Draft.Revision + 1,
			SchemaVersion: schema.Version,
			SchemaHash:    schema.Hash,
			Fields:        fields,
			Tags:          tags,
			CreatedAt:     item.Draft.CreatedAt,
			UpdatedAt:     now,
		}
		item.UpdatedAt = now
		return true, nil
	}
	// Outside the tree the draft is replaced only if it is still the one checked, so a
	// concurrent write is never overwritten and ExpectedRevision holds.
	var (
		item    *ContentItem
		changed bool
	)
	if e.onTree(loaded) {
		item, changed, err = e.write(ctx, input.Match, "restore failed", restore)
	} else {
		item, changed, err = e.casWrite(ctx, input.Match, loaded, "restore failed", restore)
	}
	if err != nil {
		return nil, err
	}
	if changed {
		e.emit(item, OpSaveDraft)
	}
	return &RestoreVersionResult{Item: item, DroppedFields: droppedFields, DroppedTags: droppedTags}, nil
}

// restorableFields returns the field values of a version that the type's current schema
// takes (known fields, supported locales, and only the default locale of a non-localized
// field), with decoded documents and arrays turned into plain maps and slices, and the
// field locales it leaves out.
func (e *Engine) restorableFields(schema ContentTypeSchema, fields ContentFields) (ContentFields, []FieldLocale) {
	out := make(ContentFields, len(fields))
	var dropped []FieldLocale
	for _, fieldID := range slices.Sorted(maps.Keys(fields)) {
		fd, known := schema.field(FieldID(fieldID))
		if known {
			out[fieldID] = LocalizedFieldValues{}
		}
		for _, locale := range slices.Sorted(maps.Keys(fields[fieldID])) {
			if !known || !e.locales.isSupported(locale) || (!fd.Localized && locale != e.locales.Default) {
				dropped = append(dropped, FieldLocale{FieldID: fieldID, Locale: locale})
				continue
			}
			out[fieldID][locale] = plainValue(fields[fieldID][locale])
		}
	}
	return out, dropped
}

// plainValue turns the documents and arrays a value decodes to from MongoDB into the plain
// maps and slices that field validation and JSON input use.
func plainValue(v any) any {
	switch v := v.(type) {
	case bson.M:
		return plainMap(v)
	case map[string]any:
		return plainMap(v)
	case bson.D:
		m := make(map[string]any, len(v))
		for _, element := range v {
			m[element.Key] = plainValue(element.Value)
		}
		return m
	case bson.A:
		return plainSlice(v)
	case []any:
		return plainSlice(v)
	default:
		return v
	}
}

func plainMap[M ~map[string]any](in M) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = plainValue(v)
	}
	return out
}

func plainSlice[S ~[]any](in S) []any {
	out := make([]any, len(in))
	for i, v := range in {
		out[i] = plainValue(v)
	}
	return out
}

// restorableTags returns the tags of a version that the type's current tag policy accepts
// (a tag of a namespace that no longer takes weights weighs 1), and the tags it leaves out.
// More accepted tags than the policy allows fail the restore.
func restorableTags(schema ContentTypeSchema, tags TagSet) (TagSet, []string, error) {
	policy := schema.Tags
	out := TagSet{}
	var dropped []string
	for _, id := range slices.Sorted(maps.Keys(tags)) {
		if policy == nil {
			dropped = append(dropped, id)
			continue
		}
		weight := new(tags[id])
		name, _ := splitTagID(id)
		if namespace, ok := policy.namespace(name); ok && !namespace.Weighted {
			weight = nil
		}
		w, err := policy.checkTag(id, weight)
		if err != nil {
			dropped = append(dropped, id)
			continue
		}
		out[id] = w
	}
	if policy != nil && len(out) > policy.maxTags() {
		return nil, nil, badRequest(fmt.Sprintf("the version carries %d tags, an item of type %q at most %d", len(out), schema.Type, policy.maxTags()))
	}
	if len(out) == 0 {
		out = nil
	}
	return out, dropped, nil
}
