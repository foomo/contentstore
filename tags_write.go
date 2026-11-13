package contentstore

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"
)

// ContentTags is the tag API of the engine. *Engine implements it. It is separate from
// ContentStore so existing implementations of that interface keep compiling.
type ContentTags interface {
	UpdateTags(ctx context.Context, input UpdateTagsInput) (*ContentItem, error)
	ListTagged(ctx context.Context, view SnapshotView, input ListTaggedInput) (*ItemPage, error)
	GetTagCounts(ctx context.Context, view SnapshotView, input TagCountsInput) (*TagCounts, error)
	MergeTags(ctx context.Context, input MergeTagsInput) (*MergeTagsResult, error)
}

// mergeTagsBatchSize is how many carriers of the merged tag one ranking query fetches.
const mergeTagsBatchSize = 1000

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

// MergeTags merges tag From into tag To on every live item of Types that carries From, in
// its draft and its published snapshot, e.g. to fix a misspelled tag. An item carrying
// both keeps the higher weight; To weighs 1 in an unweighted namespace. Items are merged
// one at a time, each as a compare-and-swap on both snapshot revisions retried on
// conflict, so a merge is not atomic, but repeating it completes an interrupted one. A
// merge bumps the draft revision but keeps the item's state; it emits OpMergeTags for
// items whose published tags changed and OpSaveDraft for the others. Archived items keep
// From. Items whose policy rejects To are reported in Failed; a store error aborts.
func (e *Engine) MergeTags(ctx context.Context, input MergeTagsInput) (*MergeTagsResult, error) {
	// From may be any stored key, like a removed tag, so malformed imported tags can be
	// merged away too.
	if input.From == "" {
		return nil, badRequest("from is required")
	}
	if err := validateTagID(input.To); err != nil {
		return nil, badRequest(err.Error())
	}
	if input.From == input.To {
		return nil, badRequest("from and to must differ")
	}
	if e.tags == nil {
		return nil, internalErr(nil, "store does not support tags")
	}
	types := input.Types
	if len(types) == 0 {
		if types = e.taggedTypes(); len(types) == 0 {
			return &MergeTagsResult{}, nil
		}
	}
	ids, err := e.tagCarriers(ctx, input.From, types)
	if err != nil {
		return nil, err
	}
	result := &MergeTagsResult{}
	for _, id := range ids {
		item, op, err := e.mergeItemTags(ctx, id, input.From, input.To)
		var failure *Error
		switch {
		case errors.As(err, &failure) && failure.Kind != KindInternal:
			result.Failed = append(result.Failed, MergeTagsFailure{ID: id, Error: failure.Msg})
		case err != nil:
			return nil, err
		case item != nil:
			result.Merged++
			e.emit(item, op)
		}
	}
	return result, nil
}

// tagCarriers returns the IDs of the live items of types carrying tag in their draft or
// published snapshot, collected before any is changed so the scan never sees its own
// writes.
func (e *Engine) tagCarriers(ctx context.Context, tag string, types []ContentType) ([]string, error) {
	seen := map[string]bool{}
	var ids []string
	for _, view := range []SnapshotView{SnapshotViewPreview, SnapshotViewPublished} {
		q := TagQuery{Tag: tag, View: view, Types: types, Fields: []FieldID{}, Limit: mergeTagsBatchSize}
		for {
			rows, err := e.tags.FindTagged(ctx, q)
			if err != nil {
				return nil, internalErr(err, "find tagged items failed")
			}
			for _, row := range rows {
				if !seen[row.ID] {
					seen[row.ID] = true
					ids = append(ids, row.ID)
				}
			}
			if len(rows) < q.Limit {
				break
			}
			last := rows[len(rows)-1]
			q.After = &TagCursor{Weight: snapshotTags(last, view)[tag], ID: last.ID}
		}
	}
	return ids, nil
}

// mergeItemTags merges from into to on one item. It returns the written item and the op to
// emit, or no item when there is nothing (left) to merge: the item is gone, archived, or no
// longer carries from.
func (e *Engine) mergeItemTags(ctx context.Context, id, from, to string) (*ContentItem, string, error) {
	var op string
	item, err := retryConflicts(func() (*ContentItem, bool, error) {
		item, err := e.resolveOptional(ctx, ContentItemMatch{ID: id})
		if err != nil || item == nil || item.ArchivedAt != nil {
			return nil, false, err
		}
		if item.Draft == nil {
			return nil, false, notAcceptable("item has no draft")
		}
		schema, _ := e.schemas.schemaFor(item.Type)
		draftTags, err := mergeTagInto(item.Type, schema.Tags, item.Draft.Tags, from, to)
		if err != nil {
			return nil, false, err
		}
		merge := TagMerge{
			ID:               item.ID,
			ExpectedRevision: item.Draft.Revision,
			DraftTags:        draftTags,
			SchemaVersion:    schema.Version,
			SchemaHash:       schema.Hash,
			Now:              time.Now().UTC(),
		}
		if item.Published != nil {
			merge.ExpectedPublishedRevision = new(item.Published.Revision)
			if merge.PublishedTags, err = mergeTagInto(item.Type, schema.Tags, item.Published.Tags, from, to); err != nil {
				return nil, false, err
			}
		}
		if merge.DraftTags == nil && merge.PublishedTags == nil {
			return nil, false, nil
		}
		written, err := e.tags.MergeTags(ctx, merge)
		if errors.Is(err, ErrConflict) {
			return nil, true, nil
		}
		if err != nil {
			return nil, false, internalErr(err, "merge tags failed")
		}
		op = OpSaveDraft
		if merge.PublishedTags != nil {
			op = OpMergeTags
		}
		return written, false, nil
	})
	return item, op, err
}

// mergeTagInto returns tags with from merged into to, or nil when tags lack from. To keeps
// the higher weight when tags carry both, weighs 1 in an unweighted namespace, and is
// checked against the policy like any tag being set. The tag count never grows, so the
// policy's limit is not checked.
func mergeTagInto(typ ContentType, policy *TagPolicy, tags TagSet, from, to string) (TagSet, error) {
	weight, ok := tags[from]
	if !ok {
		return nil, nil
	}
	if policy == nil {
		return nil, badRequest(fmt.Sprintf("type %q does not take tags", typ))
	}
	if current, ok := tags[to]; ok {
		weight = max(weight, current)
	}
	var given *float64
	if name, _ := splitTagID(to); slices.ContainsFunc(policy.Namespaces, func(ns TagNamespace) bool { return ns.Name == name && ns.Weighted }) {
		given = &weight
	}
	w, err := policy.checkTag(to, given)
	if err != nil {
		return nil, badRequest(err.Error())
	}
	next := maps.Clone(tags)
	delete(next, from)
	next[to] = w
	return next, nil
}
