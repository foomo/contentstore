package contentstore

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ContentStore is the generic, transport-neutral interface for interacting with
// content. *Engine implements it; a transport binding (e.g. gotsrpc M2M) wraps
// it and maps the returned *Error to its own error model.
type ContentStore interface {
	SaveDraft(ctx context.Context, input SaveDraftInput) (*ContentItem, error)
	SetFieldValue(ctx context.Context, input SetFieldValueInput) (*ContentItem, error)
	Publish(ctx context.Context, match ContentItemMatch) (*ContentItem, error)
	Unpublish(ctx context.Context, match ContentItemMatch) (*ContentItem, error)
	Archive(ctx context.Context, match ContentItemMatch) (*ContentItem, error)
	Unarchive(ctx context.Context, match ContentItemMatch) (*ContentItem, error)
	Delete(ctx context.Context, match ContentItemMatch) (bool, error)
	GetItem(ctx context.Context, match ContentItemMatch) (*ContentItem, error)
	GetSnapshot(ctx context.Context, view SnapshotView, input SnapshotInput) (*Snapshot, error)
	GetArchivedSnapshot(ctx context.Context, input ArchivedSnapshotInput) (*ArchivedSnapshot, error)
	GetSchemas(ctx context.Context) (*SchemasResult, error)
	ExportContent(ctx context.Context) (*ContentExport, error)
	ReplaceContent(ctx context.Context, export ContentExport) (*ReplaceContentResult, error)
}

// Engine is the content store's business logic over an injected Store, schema
// Registry, and LocaleConfig. It returns typed *Error values (never a transport
// error type).
type Engine struct {
	store   Store
	schemas *Registry
	locales LocaleConfig
	newID   func() string
	notify  Notifier
	role    DeploymentRole
}

var _ ContentStore = (*Engine)(nil)

// New builds an Engine from its dependencies.
func New(d Deps) *Engine {
	role := d.Role
	if role == "" {
		role = DeploymentRoleCanonical
	}
	return &Engine{
		store:   d.Store,
		schemas: d.Schemas,
		locales: d.Locales,
		newID:   d.NewID,
		notify:  d.Notify,
		role:    role,
	}
}

// SaveDraft creates or updates an item's draft snapshot. It does not publish.
func (e *Engine) SaveDraft(ctx context.Context, input SaveDraftInput) (*ContentItem, error) {
	// A SaveDraft replaces the draft's entire field set. Guard against an omitted
	// "fields" (nil) so a fields-less request cannot silently wipe an existing draft —
	// callers that intend to clear all fields must send an explicit empty object.
	if input.Fields == nil {
		return nil, badRequest("fields is required (send an explicit empty object {} to clear all fields)")
	}
	if input.ExpectedRevision != nil && *input.ExpectedRevision < 0 {
		return nil, badRequest("expectedRevision must not be negative")
	}

	item, err := e.resolveForSave(ctx, input)
	if err != nil {
		return nil, err
	}
	if err := e.validateDraft(item.Type, input.Fields); err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	schema, _ := e.schemas.schemaFor(item.Type)

	revision := int64(1)
	createdAt := now
	if item.Draft != nil {
		revision = item.Draft.Revision + 1
		createdAt = item.Draft.CreatedAt
	}
	item.Draft = &ContentSnapshot{
		Revision:      revision,
		SchemaVersion: schema.Version,
		SchemaHash:    schema.Hash,
		Fields:        input.Fields,
		CreatedAt:     createdAt,
		UpdatedAt:     now,
	}
	if input.OwnerRef != nil {
		item.OwnerRef = input.OwnerRef
	}
	item.UpdatedAt = now

	if err := e.store.Upsert(ctx, item, input.ExpectedRevision); err != nil {
		if errors.Is(err, ErrConflict) {
			return nil, conflict("draft revision conflict")
		}
		return nil, internalErr(err, "save draft failed")
	}
	e.emit(item, OpSaveDraft)
	return item, nil
}

// SetFieldValue merges a single field/locale value into the item's draft, leaving other
// fields untouched. It creates the item when the match resolves to nothing and Type is
// given. A nil value clears that locale. This is the partial-write counterpart to
// SaveDraft (which replaces the whole field set) and backs per-field editing.
func (e *Engine) SetFieldValue(ctx context.Context, input SetFieldValueInput) (*ContentItem, error) {
	if input.FieldID == "" || input.Locale == "" {
		return nil, badRequest("fieldID and locale are required")
	}

	// Resolve the (immutable) content type and enforce the create/update guards. This read
	// is safe under concurrency — an item's type never changes — and the write below is a
	// targeted atomic merge, so simultaneous per-field writes do not clobber each other.
	contentType, err := e.fieldWriteType(ctx, input)
	if err != nil {
		return nil, err
	}
	if err := e.validateField(contentType, input.FieldID, input.Locale, input.Value); err != nil {
		return nil, err
	}

	schema, _ := e.schemas.schemaFor(contentType)
	item, err := e.store.SetField(ctx, FieldUpdate{
		Match:         input.Match,
		Type:          contentType,
		OwnerRef:      input.OwnerRef,
		FieldID:       input.FieldID,
		Locale:        input.Locale,
		Value:         input.Value,
		SchemaVersion: schema.Version,
		SchemaHash:    schema.Hash,
		Now:           time.Now().UTC(),
		NewID:         e.newID(),
	})
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, notFound("item not found")
		}
		return nil, internalErr(err, "set field value failed")
	}
	e.emit(item, OpSaveDraft)
	return item, nil
}

// fieldWriteType resolves the content type a SetFieldValue writes against, enforcing that
// an existing item's type is immutable and that a create carries a key + known type.
func (e *Engine) fieldWriteType(ctx context.Context, input SetFieldValueInput) (ContentType, error) {
	existing, err := e.resolveExisting(ctx, input.Match)
	if err != nil {
		return "", err
	}
	if existing != nil {
		if err := rejectArchived(existing); err != nil {
			return "", err
		}
		if input.Type != "" && input.Type != existing.Type {
			return "", badRequest("item type cannot change")
		}
		return existing.Type, nil
	}
	if input.Match.Key == "" {
		return "", badRequest("key is required to create a new item")
	}
	if input.Type == "" {
		return "", badRequest("type is required to create a new item")
	}
	if _, ok := e.schemas.schemaFor(input.Type); !ok {
		return "", badRequest(fmt.Sprintf("unknown content type %q", input.Type))
	}
	return input.Type, nil
}

// Publish copies the current draft snapshot into the published snapshot.
func (e *Engine) Publish(ctx context.Context, match ContentItemMatch) (*ContentItem, error) {
	item, err := e.loadByMatch(ctx, match)
	if err != nil {
		return nil, err
	}
	if err := rejectArchived(item); err != nil {
		return nil, err
	}
	if item.Draft == nil {
		return nil, notAcceptable("nothing to publish: item has no draft")
	}
	if err := e.validatePublish(item.Type, item.Draft.Fields); err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	published := *item.Draft // published revision equals the draft revision → Published state
	published.UpdatedAt = now
	if item.Published != nil {
		published.CreatedAt = item.Published.CreatedAt
	} else {
		published.CreatedAt = now
	}
	item.Published = &published
	item.UpdatedAt = now

	if err := e.store.Upsert(ctx, item, nil); err != nil {
		return nil, internalErr(err, "publish failed")
	}
	e.emit(item, OpPublish)
	return item, nil
}

// Unpublish drops the published snapshot, returning the item to Draft while keeping the
// working draft. Unlike Unarchive it leaves ArchivedAt untouched, so it is the plain
// "take it off the live site" counterpart to Publish. An item that is not published is
// left alone (and emits nothing) rather than rejected, so bulk callers can apply it to a
// mixed selection.
func (e *Engine) Unpublish(ctx context.Context, match ContentItemMatch) (*ContentItem, error) {
	item, err := e.loadByMatch(ctx, match)
	if err != nil {
		return nil, err
	}
	if err := rejectArchived(item); err != nil {
		return nil, err
	}
	if item.Published == nil {
		return item, nil
	}
	item.Published = nil
	item.UpdatedAt = time.Now().UTC()
	// Upsert replaces the whole document; a partial $set could not clear the omitempty
	// published field (see Unarchive and archive_integration_test.go).
	if err := e.store.Upsert(ctx, item, nil); err != nil {
		return nil, internalErr(err, "unpublish failed")
	}
	e.emit(item, OpUnpublish)
	return item, nil
}

// rejectArchived blocks the write operations an archived item must not accept. Archiving
// freezes an item: it may only be unarchived, deleted, or read (mirroring Contentful,
// where an archived entry cannot be edited or published). Without this an archived item
// could take draft writes and even a publish, whose snapshot Unarchive would then discard.
func rejectArchived(item *ContentItem) error {
	if item != nil && item.ArchivedAt != nil {
		return notAcceptable("item is archived: unarchive it first")
	}
	return nil
}

// Archive hides an item while keeping both snapshots.
func (e *Engine) Archive(ctx context.Context, match ContentItemMatch) (*ContentItem, error) {
	item, err := e.loadByMatch(ctx, match)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	item.ArchivedAt = &now
	item.UpdatedAt = now
	if err := e.store.Upsert(ctx, item, nil); err != nil {
		return nil, internalErr(err, "archive failed")
	}
	e.emit(item, OpArchive)
	return item, nil
}

// Unarchive restores an archived item to Draft. It drops the published snapshot: a
// previously-published item is NOT re-published automatically (symmetric to Contentful,
// where archived entries are never published). The working draft is preserved, so no
// content is lost; re-publish to make it live again.
func (e *Engine) Unarchive(ctx context.Context, match ContentItemMatch) (*ContentItem, error) {
	item, err := e.loadByMatch(ctx, match)
	if err != nil {
		return nil, err
	}
	item.ArchivedAt = nil
	item.Published = nil
	item.UpdatedAt = time.Now().UTC()
	if err := e.store.Upsert(ctx, item, nil); err != nil {
		return nil, internalErr(err, "unarchive failed")
	}
	e.emit(item, OpUnarchive)
	return item, nil
}

// Delete hard-deletes an item. Deleting a missing item is idempotent (returns false).
func (e *Engine) Delete(ctx context.Context, match ContentItemMatch) (bool, error) {
	if match.ID == "" && match.Key == "" {
		return false, badRequest("delete requires a non-empty match (id or key)")
	}
	item, err := e.resolveOptional(ctx, match)
	if err != nil {
		return false, err
	}
	if item == nil {
		return false, nil
	}
	if err := e.store.Delete(ctx, item.ID); err != nil {
		return false, internalErr(err, "delete failed")
	}
	e.emit(item, OpDelete)
	return true, nil
}

// emit reports a content change to the injected Notifier (best-effort; nil = no-op).
// Types whose schema sets SuppressChangeEvents never notify.
func (e *Engine) emit(item *ContentItem, op string) {
	if e.notify == nil || item == nil {
		return
	}
	if s, ok := e.schemas.schemaFor(item.Type); ok && s.SuppressChangeEvents {
		return
	}
	e.notify(ContentChange{Type: item.Type, OwnerRef: item.OwnerRef, Op: op})
}

// GetItem returns a single item by match.
func (e *Engine) GetItem(ctx context.Context, match ContentItemMatch) (*ContentItem, error) {
	return e.loadByMatch(ctx, match)
}

// GetSnapshot returns the compact consumer snapshot selected by view. Published
// reads never expose working drafts; preview reads expose the latest draft and
// derived lifecycle state.
func (e *Engine) GetSnapshot(ctx context.Context, view SnapshotView, input SnapshotInput) (*Snapshot, error) {
	var (
		items []*ContentItem
		err   error
	)
	switch view {
	case SnapshotViewPublished:
		items, err = e.store.FindPublished(ctx, input.Types, input.OwnerRef)
	case SnapshotViewPreview:
		items, err = e.store.FindAll(ctx, input.Types, input.OwnerRef)
	default:
		return nil, badRequest(fmt.Sprintf("unsupported snapshot view %q", view))
	}
	if err != nil {
		return nil, internalErr(err, "snapshot read failed")
	}

	snapshot := &Snapshot{CreatedAt: time.Now().UTC(), Items: make([]SnapshotItem, 0, len(items))}
	for _, item := range items {
		var (
			fields    ContentFields
			state     string
			updatedAt time.Time
		)
		switch view {
		case SnapshotViewPublished:
			if item.Published == nil { // defensive; FindPublished already filters these out
				continue
			}
			fields = item.Published.Fields
			state = StatePublished
			updatedAt = item.Published.UpdatedAt
		case SnapshotViewPreview:
			if item.Draft == nil { // defensive; a non-archived item always has a draft
				continue
			}
			fields = item.Draft.Fields
			state = item.State()
			updatedAt = item.UpdatedAt
		}

		snapshot.Items = append(snapshot.Items, SnapshotItem{
			ID:        item.ID,
			Key:       item.Key,
			Type:      item.Type,
			OwnerRef:  item.OwnerRef,
			State:     state,
			Fields:    fields,
			UpdatedAt: updatedAt,
		})
		if revision := updatedAt.UnixNano(); revision > snapshot.Revision {
			snapshot.Revision = revision
		}
	}
	return snapshot, nil
}

// GetArchivedSnapshot returns all archived content, optionally narrowed by type — the
// counterpart to the consumer snapshot views, which both exclude archived items. It
// carries each item's working (draft) fields and ArchivedAt, for editorial views that
// restore or hard-delete archived content.
func (e *Engine) GetArchivedSnapshot(ctx context.Context, input ArchivedSnapshotInput) (*ArchivedSnapshot, error) {
	items, err := e.store.FindArchived(ctx, input.Types)
	if err != nil {
		return nil, internalErr(err, "archived snapshot read failed")
	}
	snapshot := &ArchivedSnapshot{CreatedAt: time.Now().UTC(), Items: make([]ArchivedContentItem, 0, len(items))}
	for _, item := range items {
		if item.Draft == nil || item.ArchivedAt == nil { // defensive; the store filters archived items
			continue
		}
		snapshot.Items = append(snapshot.Items, ArchivedContentItem{
			ID:         item.ID,
			Key:        item.Key,
			Type:       item.Type,
			OwnerRef:   item.OwnerRef,
			Fields:     item.Draft.Fields,
			ArchivedAt: *item.ArchivedAt,
			UpdatedAt:  item.UpdatedAt,
		})
		// Revision is the newest item UpdatedAt so an unchanged snapshot is stable.
		if rev := item.UpdatedAt.UnixNano(); rev > snapshot.Revision {
			snapshot.Revision = rev
		}
	}
	return snapshot, nil
}

// GetSchemas exposes the registered content schemas for tooling and editors.
func (e *Engine) GetSchemas(_ context.Context) (*SchemasResult, error) {
	return &SchemasResult{Schemas: e.schemas.all()}, nil
}

// resolveForSave resolves the item to update, or builds a new one to create.
func (e *Engine) resolveForSave(ctx context.Context, input SaveDraftInput) (*ContentItem, error) {
	existing, err := e.resolveExisting(ctx, input.Match)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		if err := rejectArchived(existing); err != nil {
			return nil, err
		}
		if input.Type != "" && input.Type != existing.Type {
			return nil, badRequest("item type cannot change")
		}
		if existing.Key == "" && input.Match.Key != "" {
			existing.Key = input.Match.Key
		}
		return existing, nil
	}

	// Create.
	if input.Type == "" {
		return nil, badRequest("type is required to create a new item")
	}
	if _, ok := e.schemas.schemaFor(input.Type); !ok {
		return nil, badRequest(fmt.Sprintf("unknown content type %q", input.Type))
	}
	now := time.Now().UTC()
	return &ContentItem{
		ID:        e.newID(),
		Key:       input.Match.Key,
		Type:      input.Type,
		OwnerRef:  input.OwnerRef,
		CreatedAt: now,
		UpdatedAt: now,
	}, nil
}

// resolveExisting loads the item addressed by a match for an update. An ID that does not
// resolve is an error (update semantics); a Key that does not resolve returns nil (an
// upsert-by-key create). When both are given they must resolve to the same item.
func (e *Engine) resolveExisting(ctx context.Context, m ContentItemMatch) (*ContentItem, error) {
	var byID, byKey *ContentItem
	if m.ID != "" {
		it, err := e.store.GetByID(ctx, m.ID)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				return nil, notFound(fmt.Sprintf("item %q not found", m.ID))
			}
			return nil, internalErr(err, "load by id failed")
		}
		byID = it
	}
	if m.Key != "" {
		it, err := e.store.GetByKey(ctx, m.Key)
		if err != nil {
			if !errors.Is(err, ErrNotFound) {
				return nil, internalErr(err, "load by key failed")
			}
		} else {
			byKey = it
		}
	}
	if byID != nil && byKey != nil && byID.ID != byKey.ID {
		return nil, badRequest("id and key resolve to different items")
	}
	if byID != nil {
		return byID, nil
	}
	return byKey, nil
}

// loadByMatch loads exactly one item; a non-empty match that does not resolve is a
// not-found error.
func (e *Engine) loadByMatch(ctx context.Context, m ContentItemMatch) (*ContentItem, error) {
	if m.ID == "" && m.Key == "" {
		return nil, badRequest("match requires id or key")
	}
	item, err := e.resolveOptional(ctx, m)
	if err != nil {
		return nil, err
	}
	if item == nil {
		return nil, notFound("item not found")
	}
	return item, nil
}

// resolveOptional loads the matched item, returning (nil, nil) when it does not exist.
// When both ID and Key are given and both exist, they must resolve to the same item.
func (e *Engine) resolveOptional(ctx context.Context, m ContentItemMatch) (*ContentItem, error) {
	var byID, byKey *ContentItem
	if m.ID != "" {
		it, err := e.store.GetByID(ctx, m.ID)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return nil, internalErr(err, "load by id failed")
		}
		byID = it
	}
	if m.Key != "" {
		it, err := e.store.GetByKey(ctx, m.Key)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return nil, internalErr(err, "load by key failed")
		}
		byKey = it
	}
	if byID != nil && byKey != nil && byID.ID != byKey.ID {
		return nil, badRequest("id and key resolve to different items")
	}
	if byID != nil {
		return byID, nil
	}
	return byKey, nil
}
