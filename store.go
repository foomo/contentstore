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
