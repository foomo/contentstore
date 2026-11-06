package contentstore

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"time"
)

// ContentTags is the tag API of the engine. *Engine implements it. It is separate from
// ContentStore so existing implementations of that interface keep compiling.
type ContentTags interface {
	UpdateTags(ctx context.Context, input UpdateTagsInput) (*ContentItem, error)
	ListTagged(ctx context.Context, view SnapshotView, input ListTaggedInput) (*ItemPage, error)
	GetTagCounts(ctx context.Context, view SnapshotView, input TagCountsInput) (*TagCounts, error)
}

var _ ContentTags = (*Engine)(nil)

// UpdateTags changes an item's draft tags. It is a compare-and-swap on the draft revision,
// retried from a fresh load when a concurrent write wins, so the tag limit holds exactly
// and no concurrent change is lost. It bumps the draft revision (a published item becomes
// changed) and emits OpSaveDraft; a write that changes nothing is a no-op.
func (e *Engine) UpdateTags(ctx context.Context, input UpdateTagsInput) (*ContentItem, error) {
	if input.Match.ID == "" && input.Match.Key == "" {
		return nil, badRequest("match requires id or key")
	}
	if input.Replace && len(input.Remove) > 0 {
		return nil, badRequest("remove cannot be combined with replace")
	}
	for _, id := range input.Remove {
		// Removal takes any stored key, so a malformed imported tag can still be dropped.
		if id == "" {
			return nil, badRequest("removed tag IDs must not be empty")
		}
		if _, ok := input.Set[id]; ok {
			return nil, badRequest(fmt.Sprintf("tag %q is both set and removed", id))
		}
	}
	if e.tags == nil {
		return nil, internalErr(nil, "store does not support tags")
	}
	var written bool
	item, err := retryConflicts(func() (*ContentItem, bool, error) {
		item, err := e.loadByMatch(ctx, input.Match)
		if err != nil {
			return nil, false, err
		}
		if err := rejectArchived(item); err != nil {
			return nil, false, err
		}
		if item.Draft == nil {
			return nil, false, notAcceptable("item has no draft")
		}
		schema, ok := e.schemas.schemaFor(item.Type)
		if !ok {
			schema = ContentTypeSchema{Type: item.Type, Version: item.Draft.SchemaVersion, Hash: item.Draft.SchemaHash}
		}
		tags, err := mergeTags(schema, item.Draft.Tags, input)
		if err != nil {
			return nil, false, err
		}
		if maps.Equal(tags, item.Draft.Tags) {
			return item, false, nil
		}
		updated, err := e.tags.UpdateTags(ctx, TagUpdate{
			ID:               item.ID,
			ExpectedRevision: item.Draft.Revision,
			Tags:             tags,
			SchemaVersion:    schema.Version,
			SchemaHash:       schema.Hash,
			Now:              time.Now().UTC(),
		})
		if errors.Is(err, ErrConflict) {
			return nil, true, nil
		}
		if err != nil {
			return nil, false, internalErr(err, "update tags failed")
		}
		written = true
		return updated, false, nil
	})
	if err != nil {
		return nil, err
	}
	if written {
		e.emit(item, OpSaveDraft)
	}
	return item, nil
}

// mergeTags computes an item's new tag set: the current tags (unless replacing) without the
// removed ones, plus the set ones checked against the type's policy. Tags already present
// stay as they are even if the policy has narrowed since, and an item above the tag limit
// can still drop or re-weigh tags, just not gain new ones.
func mergeTags(schema ContentTypeSchema, current TagSet, input UpdateTagsInput) (TagSet, error) {
	policy := schema.Tags
	if policy == nil && len(input.Set) > 0 {
		return nil, badRequest(fmt.Sprintf("type %q does not take tags", schema.Type))
	}
	next := TagSet{}
	if !input.Replace {
		maps.Copy(next, current)
	}
	for _, id := range input.Remove {
		delete(next, id)
	}
	adds := false
	for id, weight := range input.Set {
		w, err := policy.checkTag(id, weight)
		if err != nil {
			return nil, badRequest(err.Error())
		}
		if _, ok := current[id]; !ok {
			adds = true
		}
		next[id] = w
	}
	if policy != nil && adds && len(next) > policy.maxTags() {
		return nil, badRequest(fmt.Sprintf("an item of type %q carries at most %d tags", schema.Type, policy.maxTags()))
	}
	return next, nil
}
