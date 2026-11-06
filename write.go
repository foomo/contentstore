package contentstore

import (
	"context"
	"errors"
)

// maxWriteAttempts bounds the compare-and-swap retries of a write that keeps losing to
// concurrent writes of the same item.
const maxWriteAttempts = 5

// mutation changes a loaded item in memory and reports whether it must be written back.
type mutation func(ctx context.Context, item *ContentItem) (bool, error)

// write loads the matched item, applies mutate and replaces the stored document when it
// changed. How it guards the replace depends on the item:
//   - tree items: inside a tree transaction (see treeWrite);
//   - items that take tags: compare-and-swap on the loaded draft revision, retried from a
//     fresh load, so a concurrent targeted write (UpdateTags, SetFieldValue) is never
//     overwritten;
//   - every other item: directly, exactly as before trees and tags existed.
//
// failure is the message of an unexpected store error.
func (e *Engine) write(ctx context.Context, match ContentItemMatch, failure string, mutate mutation) (*ContentItem, bool, error) {
	item, err := e.loadByMatch(ctx, match)
	if err != nil {
		return nil, false, err
	}
	switch {
	case e.onTree(item):
		var changed bool
		item, err = e.treeWrite(ctx, match, func(ctx context.Context, item *ContentItem) error {
			var err error
			if changed, err = mutate(ctx, item); err != nil || !changed {
				return err
			}
			if err := e.store.Upsert(ctx, item, nil); err != nil {
				return internalErr(err, failure)
			}
			return nil
		})
		if err != nil {
			return nil, false, err
		}
		return item, changed, nil
	case e.tagged(item):
		return e.casWrite(ctx, match, item, failure, mutate)
	}
	changed, err := mutate(ctx, item)
	if err != nil {
		return nil, false, err
	}
	if changed {
		if err := e.store.Upsert(ctx, item, nil); err != nil {
			return nil, false, internalErr(err, failure)
		}
	}
	return item, changed, nil
}

// casWrite applies mutate to item and replaces the stored document only if its draft
// revision is still the loaded one, reloading and retrying otherwise.
func (e *Engine) casWrite(ctx context.Context, match ContentItemMatch, item *ContentItem, failure string, mutate mutation) (*ContentItem, bool, error) {
	for attempt := 1; ; attempt++ {
		var expected *int64
		if item.Draft != nil {
			expected = new(item.Draft.Revision)
		}
		changed, err := mutate(ctx, item)
		if err != nil {
			return nil, false, err
		}
		if !changed {
			return item, false, nil
		}
		err = e.store.Upsert(ctx, item, expected)
		if err == nil {
			return item, true, nil
		}
		if !errors.Is(err, ErrConflict) {
			return nil, false, internalErr(err, failure)
		}
		if attempt == maxWriteAttempts {
			return nil, false, conflict("item keeps changing concurrently: retry later")
		}
		if item, err = e.loadByMatch(ctx, match); err != nil {
			return nil, false, err
		}
	}
}

// tagged reports whether item's whole-document writes must compare-and-swap: its type
// takes tags, or it still carries some, so targeted tag writes can race with them.
func (e *Engine) tagged(item *ContentItem) bool {
	if item.Draft != nil && len(item.Draft.Tags) > 0 {
		return true
	}
	schema, ok := e.schemas.schemaFor(item.Type)
	return ok && schema.Tags != nil
}
