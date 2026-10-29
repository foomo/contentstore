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

// ChildLifecycle selects the lifecycle states FindChildren returns.
type ChildLifecycle int

const (
	// ChildrenAny returns children in every state, archived included.
	ChildrenAny ChildLifecycle = iota
	// ChildrenLive returns non-archived children.
	ChildrenLive
	// ChildrenPublished returns published, non-archived children.
	ChildrenPublished
)

// ChildQuery selects the children of one parent.
type ChildQuery struct {
	// ParentID is the parent's ID; empty selects named root-level items.
	ParentID string
	// Name selects the child with exactly this name when set.
	Name string
	// AfterName selects children whose name sorts after it (keyset pagination).
	AfterName string
	Lifecycle ChildLifecycle
	// Types narrows the children by content type (all when empty).
	Types []ContentType
	// Fields projects the draft and published field sets: nil keeps every field, an empty
	// slice keeps none (structure, placement, lifecycle and revisions only).
	Fields []FieldID
	// Limit caps the number of children (unlimited when zero).
	Limit int
}
