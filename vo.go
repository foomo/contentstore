package contentstore

import "time"

const ContentExportFormatVersion = 1

type (
	// ContentType identifies a content schema (e.g. "seoOverride").
	ContentType string
	// FieldID identifies a field within a schema (e.g. "metaTitle").
	FieldID string
	// FieldType is a supported field type (see schema.go).
	FieldType string
	// FieldValue is a single field's value for one locale: a string for symbol/text,
	// an integer-compatible number, or a JSON object for richtext/json.
	FieldValue = any
)

// Derived publishing states (never stored as an enum; see ContentItem.State).
const (
	StateDraft     = "draft"
	StateChanged   = "changed"
	StatePublished = "published"
	StateArchived  = "archived"
)

// SnapshotView selects which persisted representation is visible through the
// consumer snapshot API.
type SnapshotView string

const (
	SnapshotViewPublished SnapshotView = "published"
	SnapshotViewPreview   SnapshotView = "preview"
)

// Valid reports whether v identifies a supported consumer snapshot view.
func (v SnapshotView) Valid() bool {
	return v == SnapshotViewPublished || v == SnapshotViewPreview
}

// ContentFields maps fieldID -> locale code -> value.
type ContentFields map[string]LocalizedFieldValues

// LocalizedFieldValues maps locale code -> value. Non-localized fields carry only the
// default-locale key.
type LocalizedFieldValues map[string]FieldValue

// OwnerRef identifies the primary external entity this content is associated with. It
// is ownership/reconciliation metadata, not a generic reference system.
type OwnerRef struct {
	Source string `json:"source" bson:"source"`
	Type   string `json:"type" bson:"type"`
	ID     string `json:"id" bson:"id"`
}

// ContentSnapshot is one revision of an item's fields (draft or published).
type ContentSnapshot struct {
	Revision      int64         `json:"revision" bson:"revision"`
	SchemaVersion string        `json:"schemaVersion" bson:"schemaVersion"`
	SchemaHash    string        `json:"schemaHash" bson:"schemaHash"`
	Fields        ContentFields `json:"fields" bson:"fields"`
	CreatedAt     time.Time     `json:"createdAt" bson:"createdAt"`
	UpdatedAt     time.Time     `json:"updatedAt" bson:"updatedAt"`
}

// ContentItem is one stored content piece, persisted as a single MongoDB document
// (ID is the Mongo _id). Draft and Published snapshots live in the same document.
//
// ParentID and Name place an item of a tree type in its content tree (see
// ContentTypeSchema.ChildTypes). Like Key and OwnerRef they are structural and not
// versioned: a move or rename applies to the draft and published views at once. An empty
// ParentID with a Name means root level; items of other types carry neither.
type ContentItem struct {
	ID         string           `json:"id" bson:"_id"`
	Key        string           `json:"key,omitempty" bson:"key,omitempty"`
	Type       ContentType      `json:"type" bson:"type"`
	OwnerRef   *OwnerRef        `json:"ownerRef,omitempty" bson:"ownerRef,omitempty"`
	ParentID   string           `json:"parentId,omitempty" bson:"parentId,omitempty"`
	Name       string           `json:"name,omitempty" bson:"name,omitempty"`
	Draft      *ContentSnapshot `json:"draft,omitempty" bson:"draft,omitempty"`
	Published  *ContentSnapshot `json:"published,omitempty" bson:"published,omitempty"`
	ArchivedAt *time.Time       `json:"archivedAt,omitempty" bson:"archivedAt,omitempty"`
	CreatedAt  time.Time        `json:"createdAt" bson:"createdAt"`
	UpdatedAt  time.Time        `json:"updatedAt" bson:"updatedAt"`
}

// ContentExport is a complete, transportable snapshot of Content Store data.
// It includes archived content and both draft and published representations.
type ContentExport struct {
	FormatVersion int            `json:"formatVersion"`
	CreatedAt     time.Time      `json:"createdAt"`
	ItemCount     int            `json:"itemCount"`
	Checksum      string         `json:"checksum"`
	Items         []*ContentItem `json:"items"`
}

// ReplaceContentResult identifies the snapshot installed in a sandbox.
type ReplaceContentResult struct {
	ItemCount int    `json:"itemCount"`
	Checksum  string `json:"checksum"`
}

// State derives the publishing state. Archived takes precedence; a live item always
// has a Draft, so Published == nil means draft-only.
func (i *ContentItem) State() string {
	switch {
	case i.ArchivedAt != nil:
		return StateArchived
	case i.Published == nil:
		return StateDraft
	case i.Draft != nil && i.Draft.Revision == i.Published.Revision:
		return StatePublished
	default:
		return StateChanged
	}
}

// ContentItemMatch selects an item by ID and/or Key.
type ContentItemMatch struct {
	ID  string `json:"id,omitempty"`
	Key string `json:"key,omitempty"`
}

// SaveDraftInput is the request for SaveDraft. Type is required only when creating.
// ExpectedRevision is optional: nil preserves unconditional save behavior, zero creates
// only when no item exists, and a positive value updates only that draft revision.
//
// ParentID and Name place a new item of a tree type (Name is required for those; an empty
// ParentID means root level). On update they may be omitted or must match the current
// placement: use Move to change it.
type SaveDraftInput struct {
	Match            ContentItemMatch `json:"match"`
	Type             ContentType      `json:"type,omitempty"`
	OwnerRef         *OwnerRef        `json:"ownerRef,omitempty"`
	ParentID         string           `json:"parentId,omitempty"`
	Name             string           `json:"name,omitempty"`
	Fields           ContentFields    `json:"fields"`
	ExpectedRevision *int64           `json:"expectedRevision,omitempty"`
}

// MoveInput moves and/or renames a tree item. A nil ParentID keeps the current parent and
// a pointer to "" moves the item to root level; an empty Name keeps the current name.
type MoveInput struct {
	Match    ContentItemMatch `json:"match"`
	ParentID *string          `json:"parentId,omitempty"`
	Name     string           `json:"name,omitempty"`
}

// SetFieldValueInput sets a single field's value for one locale, merging into the item's
// draft (other fields are left untouched). Type/OwnerRef are used only when the match
// resolves to nothing and the item must be created. A nil Value clears that locale.
type SetFieldValueInput struct {
	Match    ContentItemMatch `json:"match"`
	Type     ContentType      `json:"type,omitempty"`
	OwnerRef *OwnerRef        `json:"ownerRef,omitempty"`
	FieldID  string           `json:"fieldID"`
	Locale   string           `json:"locale"`
	Value    FieldValue       `json:"value"`
}

// SnapshotInput narrows a consumer snapshot by content type and, optionally,
// exact owner. Empty filters return every item visible in the configured view.
type SnapshotInput struct {
	Types    []ContentType `json:"types,omitempty"`
	OwnerRef *OwnerRef     `json:"ownerRef,omitempty"`
}

// Snapshot is the common bulk-read shape for published and preview consumers.
// The selected view changes visibility and fields, not the response contract.
type Snapshot struct {
	// Revision fingerprints the returned items using their latest timestamp. It
	// is not monotonic and can move backwards when the newest item is deleted;
	// consumers must not use it as a conditional-reload cursor.
	Revision  int64          `json:"revision"`
	CreatedAt time.Time      `json:"createdAt"`
	Items     []SnapshotItem `json:"items"`
}

// SnapshotItem is one compact consumer-facing item. UpdatedAt is the time at
// which the returned representation last changed: publish time for the
// published view and item update time for the preview view.
type SnapshotItem struct {
	ID        string        `json:"id"`
	Key       string        `json:"key,omitempty"`
	Type      ContentType   `json:"type"`
	OwnerRef  *OwnerRef     `json:"ownerRef,omitempty"`
	ParentID  string        `json:"parentId,omitempty"`
	Name      string        `json:"name,omitempty"`
	State     string        `json:"state"`
	Fields    ContentFields `json:"fields"`
	UpdatedAt time.Time     `json:"updatedAt"`
}

// ArchivedSnapshotInput narrows the archived snapshot to the given content types (all when empty).
type ArchivedSnapshotInput struct {
	Types []ContentType `json:"types,omitempty"`
}

// ArchivedSnapshot is the bulk read of archived content, for editorial views that let
// editors find, restore (unarchive), or hard-delete archived items — which are otherwise
// excluded from both consumer snapshot views. It carries the working (draft)
// fields and when each item was archived.
type ArchivedSnapshot struct {
	Revision  int64                 `json:"revision"`
	CreatedAt time.Time             `json:"createdAt"`
	Items     []ArchivedContentItem `json:"items"`
}

// ArchivedContentItem is one item in an ArchivedSnapshot. It exposes the draft (last
// working) fields and ArchivedAt; it deliberately omits a derived State field, which would
// always be "archived".
type ArchivedContentItem struct {
	ID         string        `json:"id"`
	Key        string        `json:"key,omitempty"`
	Type       ContentType   `json:"type"`
	OwnerRef   *OwnerRef     `json:"ownerRef,omitempty"`
	ParentID   string        `json:"parentId,omitempty"`
	Name       string        `json:"name,omitempty"`
	Fields     ContentFields `json:"fields"`
	ArchivedAt time.Time     `json:"archivedAt"`
	UpdatedAt  time.Time     `json:"updatedAt"`
}

// ListChildrenInput lists the children of ParentID (named root-level items when empty),
// ordered by name. Types narrows them by content type and Fields projects their field
// sets (all fields when empty). Limit defaults to 100 and may be at most 1000; Cursor
// continues from a previous page's NextCursor.
type ListChildrenInput struct {
	ParentID string        `json:"parentId,omitempty"`
	Types    []ContentType `json:"types,omitempty"`
	Fields   []FieldID     `json:"fields,omitempty"`
	Limit    int           `json:"limit,omitempty"`
	Cursor   string        `json:"cursor,omitempty"`
}

// ChildrenPage is one page of ListChildren. NextCursor is empty on the last page.
type ChildrenPage struct {
	Items      []SnapshotItem `json:"items"`
	NextCursor string         `json:"nextCursor,omitempty"`
}

// TreeItemInput addresses one item either by Match or by Path ("/kb/tables/users", the
// names from the root level down; the leading slash is optional). Fields projects its
// field set (all fields when empty).
type TreeItemInput struct {
	Match  ContentItemMatch `json:"match"`
	Path   string           `json:"path,omitempty"`
	Fields []FieldID        `json:"fields,omitempty"`
}

// TreeItem is one item in a snapshot view with its ancestors (root first) and its path.
// Items outside the tree have no ancestors and an empty path.
type TreeItem struct {
	Item      SnapshotItem `json:"item"`
	Ancestors []TreeRef    `json:"ancestors"`
	Path      string       `json:"path,omitempty"`
}

// TreeRef identifies an ancestor in a TreeItem.
type TreeRef struct {
	ID   string      `json:"id"`
	Type ContentType `json:"type"`
	Name string      `json:"name"`
}

// SchemasResult exposes the registered content schemas.
type SchemasResult struct {
	Schemas []ContentTypeSchema `json:"schemas"`
}
