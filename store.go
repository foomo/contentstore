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
	// MergeTags writes one item's share of a tag merge, if the item is not archived, its
	// draft revision is m.ExpectedRevision and its published revision is
	// m.ExpectedPublishedRevision (nil: it has no published snapshot). It bumps the draft
	// revision, and a published snapshot of the expected draft revision follows the bump,
	// so the item's state is unchanged. It returns ErrConflict otherwise, including when
	// the item no longer exists.
	MergeTags(ctx context.Context, m TagMerge) (*ContentItem, error)
}

// TagMerge is a compare-and-swap write of an item's tag sets after merging one tag into
// another. The published precondition matters because publishing copies the draft
// without bumping its revision.
type TagMerge struct {
	ID                        string
	ExpectedRevision          int64
	ExpectedPublishedRevision *int64
	// DraftTags and PublishedTags are the new complete sets; nil leaves a snapshot's tags
	// and its timestamp unchanged, and an empty set removes its tags (a tag deletion can
	// empty a set; a merge never does).
	DraftTags     TagSet
	PublishedTags TagSet
	SchemaVersion string
	SchemaHash    string
	Now           time.Time
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

// ScheduleStore is the optional persistence contract for scheduled actions. *Persistor
// implements it and enables it with WithSchedules; an Engine over a store without it, or
// with it disabled, rejects scheduled actions, so existing Store implementations keep
// working unchanged.
type ScheduleStore interface {
	Store
	// SchedulesEnabled reports whether the store keeps scheduled actions.
	SchedulesEnabled() bool
	// InsertScheduledAction stores a new action. It returns ErrConflict when the item
	// already has a pending action at the same time.
	InsertScheduledAction(ctx context.Context, action *ScheduledAction) error
	// GetScheduledAction returns the action with the given ID, or ErrNotFound.
	GetScheduledAction(ctx context.Context, id string) (*ScheduledAction, error)
	// FindScheduledActions returns the actions matching q, ordered by At, then ID.
	FindScheduledActions(ctx context.Context, q ScheduledActionQuery) ([]*ScheduledAction, error)
	// UpdateScheduledAction replaces the stored action if its stored status is one of from,
	// and returns ErrConflict otherwise, including when it no longer exists.
	UpdateScheduledAction(ctx context.Context, action *ScheduledAction, from ...ScheduledActionStatus) error
	// DeleteScheduledActions deletes the actions matching q's ID, ItemID and Statuses (its
	// other fields are ignored) and returns how many it deleted.
	DeleteScheduledActions(ctx context.Context, q ScheduledActionQuery) (int64, error)
	// AcquireScheduleRunner takes the single runner lease for holder until the given time,
	// or extends it when holder already has it. It returns false while another holder's
	// lease has not expired.
	AcquireScheduleRunner(ctx context.Context, holder string, until time.Time) (bool, error)
	// ReleaseScheduleRunner gives up holder's lease; it is a no-op when holder has none.
	ReleaseScheduleRunner(ctx context.Context, holder string) error
}

// ScheduledActionQuery selects scheduled actions. Every set field narrows the selection.
type ScheduledActionQuery struct {
	ID       string
	ItemID   string
	Statuses []ScheduledActionStatus
	// From and To bound At: From inclusive, To exclusive.
	From *time.Time
	To   *time.Time
	// After continues after this position in the (At, ID) order (from the start when nil).
	After *ScheduleCursor
	// Limit caps the number of actions (unlimited when zero).
	Limit int
}

// ScheduleCursor is a position in the (At, ID) order of scheduled actions.
type ScheduleCursor struct {
	At time.Time `json:"at"`
	ID string    `json:"id"`
}

// VersionStore is the optional persistence contract for version history. *Persistor
// implements it and enables it with WithVersions; an Engine over a store without it, or with
// it disabled, rejects the ContentVersions methods and publishes of types with a
// VersionPolicy, so existing Store implementations keep working unchanged.
type VersionStore interface {
	Store
	// VersionsEnabled reports whether the store keeps versions.
	VersionsEnabled() bool
	// InsertVersion stores v, unless item v.ItemID already has a version of v.Revision,
	// which it keeps unchanged. Inside a tree transaction it must not fail the transaction
	// on an existing version.
	InsertVersion(ctx context.Context, v *ContentVersion) error
	// GetVersion returns item itemID's version of the given revision, or ErrNotFound.
	GetVersion(ctx context.Context, itemID string, revision int64) (*ContentVersion, error)
	// FindVersions returns the versions matching q, newest first, without their snapshots.
	FindVersions(ctx context.Context, q VersionQuery) ([]*ContentVersion, error)
	// PruneVersions deletes item itemID's versions except the newest keep.
	PruneVersions(ctx context.Context, itemID string, keep int) error
	// DeleteVersions deletes item itemID's versions, or every version when itemID is empty.
	DeleteVersions(ctx context.Context, itemID string) error
}

// VersionQuery selects one page of an item's versions.
type VersionQuery struct {
	ItemID string
	// Before continues with the versions older than this revision (from the newest when 0).
	Before int64
	// Limit caps the number of versions (unlimited when zero).
	Limit int
}
