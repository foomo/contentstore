package contentstore

import (
	"context"
	"errors"
	"time"
)

var (
	// ErrNotFound is returned by a Store when a content item does not exist.
	ErrNotFound = errors.New("content item not found")
	// ErrConflict is returned when a conditional write precondition does not match.
	ErrConflict = errors.New("content item write conflict")
)

// Store is the persistence contract the engine depends on. *Persistor is the
// MongoDB-backed implementation; tests use a fake.
type Store interface {
	GetByID(ctx context.Context, id string) (*ContentItem, error)
	GetByKey(ctx context.Context, key string) (*ContentItem, error)
	Upsert(ctx context.Context, item *ContentItem, expectedDraftRevision *int64) error
	SetField(ctx context.Context, in FieldUpdate) (*ContentItem, error)
	Delete(ctx context.Context, id string) error
	FindPublished(ctx context.Context, types []ContentType, ownerRef *OwnerRef) ([]*ContentItem, error)
	FindAll(ctx context.Context, types []ContentType, ownerRef *OwnerRef) ([]*ContentItem, error)
	FindArchived(ctx context.Context, types []ContentType) ([]*ContentItem, error)
	ExportAll(ctx context.Context) ([]*ContentItem, error)
	ReplaceAll(ctx context.Context, items []*ContentItem) error
}

// FieldUpdate is a single-field, single-locale draft write. It is applied as a
// targeted, atomic update (see Persistor.SetField) so concurrent writers editing
// different fields do not clobber each other and a first-time write creates the
// item exactly once.
type FieldUpdate struct {
	Match         ContentItemMatch
	Type          ContentType
	OwnerRef      *OwnerRef
	FieldID       string
	Locale        string
	Value         FieldValue
	SchemaVersion string
	SchemaHash    string
	Now           time.Time
	NewID         string
}

// TreeStore is the optional persistence contract for content trees. *Persistor
// implements it; an Engine over a Store without it rejects tree operations, so existing
// Store implementations keep working unchanged.
type TreeStore interface {
	Store
	// RunTreeTransaction runs fn in a transaction serialized with every other tree
	// transaction. fn receives the transactional context, must route all its reads and
	// writes through it, and may run more than once.
	RunTreeTransaction(ctx context.Context, fn func(ctx context.Context) error) error
	// FindChildren returns the items placed under a parent, ordered by name. Results can
	// be projected (see ChildQuery.Fields) and must then never be written back.
	FindChildren(ctx context.Context, q ChildQuery) ([]*ContentItem, error)
}

// Lifecycle selects the lifecycle states a query returns.
type Lifecycle int

const (
	// LifecycleAny returns items in every state, archived included.
	LifecycleAny Lifecycle = iota
	// LifecycleLive returns non-archived items.
	LifecycleLive
	// LifecyclePublished returns published, non-archived items.
	LifecyclePublished
)

// ChildQuery selects the children of one parent.
type ChildQuery struct {
	// ParentID is the parent's ID; empty selects named root-level items.
	ParentID string
	// Name selects the child with exactly this name when set.
	Name string
	// AfterName selects children whose name sorts after it (keyset pagination).
	AfterName string
	Lifecycle Lifecycle
	// Types narrows the children by content type (all when empty).
	Types []ContentType
	// Tags narrows the children by the tags of TagView's snapshot (draft tags for the
	// preview view, published tags for the published one).
	Tags    *TagFilter
	TagView SnapshotView
	// Fields projects the draft and published field sets: nil keeps every field, an empty
	// slice keeps none (structure, placement, lifecycle and revisions only).
	Fields []FieldID
	// Limit caps the number of children (unlimited when zero).
	Limit int
}

// TagStore is the optional persistence contract for tag writes. *Persistor implements it;
// an Engine over a Store without it rejects UpdateTags, so existing Store implementations
// keep working unchanged.
type TagStore interface {
	Store
	// UpdateTags replaces the draft tag set of item u.ID and bumps its draft revision, if
	// the item is not archived and its stored draft revision is u.ExpectedRevision. It
	// returns ErrConflict otherwise, including when the item no longer exists.
	UpdateTags(ctx context.Context, u TagUpdate) (*ContentItem, error)
	// FindTagged returns non-archived items carrying q.Tag in q.View's snapshot, ranked by
	// its weight (highest first, ties by descending ID) after the keyset position q.After.
	// Results can be projected (see ChildQuery.Fields) and must then never be written back.
	FindTagged(ctx context.Context, q TagQuery) ([]*ContentItem, error)
}

// TagQuery selects one page of a tag ranking.
type TagQuery struct {
	Tag string
	// View selects the ranked snapshot: published tags of published items, or draft tags.
	View  SnapshotView
	Types []ContentType
	// After continues the ranking after this position (from the start when nil).
	After  *TagCursor
	Fields []FieldID
	Limit  int
}

// TagCursor is a position in a tag ranking: the weight and ID of the last item seen.
type TagCursor struct {
	Weight float64 `json:"w"`
	ID     string  `json:"id"`
}

// TagUpdate is a compare-and-swap write of an item's complete draft tag set.
type TagUpdate struct {
	ID               string
	ExpectedRevision int64
	// Tags is the complete new set; empty clears the tags.
	Tags          TagSet
	SchemaVersion string
	SchemaHash    string
	Now           time.Time
}
