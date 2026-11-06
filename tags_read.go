package contentstore

import (
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

const (
	defaultTagCountsLimit = 20
	// rankingScanFactor bounds how many ranked rows one ListTagged page may scan, as a
	// multiple of its limit, when Filter or unpublished ancestors hide many of them.
	rankingScanFactor = 10
)

// validate checks the filter's tag IDs; a nil filter is valid.
func (f *TagFilter) validate() error {
	if f == nil {
		return nil
	}
	for _, ids := range [][]string{f.All, f.Any, f.None} {
		for _, id := range ids {
			if err := validateTagID(id); err != nil {
				return badRequest(err.Error())
			}
		}
	}
	return nil
}

// matches reports whether tags satisfy the filter; a nil filter matches everything.
func (f *TagFilter) matches(tags TagSet) bool {
	if f == nil {
		return true
	}
	has := func(id string) bool { _, ok := tags[id]; return ok }
	if !allOf(f.All, has) || slices.ContainsFunc(f.None, has) {
		return false
	}
	return len(f.Any) == 0 || slices.ContainsFunc(f.Any, has)
}

func allOf(ids []string, has func(string) bool) bool {
	for _, id := range ids {
		if !has(id) {
			return false
		}
	}
	return true
}

// ListTagged returns one page of the items carrying a tag, ranked by its weight, highest
// first. The ranking itself is served by the store (an index with WithTagIndexes); the tag
// filter and, in the published view, hidden ancestors are applied here, scanning at most
// rankingScanFactor times the limit before returning a short page with a NextCursor.
func (e *Engine) ListTagged(ctx context.Context, view SnapshotView, input ListTaggedInput) (*ItemPage, error) {
	if _, err := viewLifecycle(view); err != nil {
		return nil, err
	}
	if err := validateTagID(input.Tag); err != nil {
		return nil, badRequest(err.Error())
	}
	if err := input.Filter.validate(); err != nil {
		return nil, err
	}
	limit := input.Limit
	switch {
	case limit == 0:
		limit = defaultChildrenLimit
	case limit < 0 || limit > maxChildrenLimit:
		return nil, badRequest(fmt.Sprintf("limit must be between 1 and %d", maxChildrenLimit))
	}
	after, err := decodeTagCursor(input.Cursor)
	if err != nil {
		return nil, err
	}
	fields, err := e.projection(input.Types, input.Fields)
	if err != nil {
		return nil, err
	}
	if e.tags == nil {
		return nil, internalErr(nil, "store does not support tags")
	}

	page := &ItemPage{Items: make([]SnapshotItem, 0, limit)}
	known := map[string]*ContentItem{}
	visible := map[string]bool{}
	var lastReturned *TagCursor
	budget := rankingScanFactor * limit
	for {
		// One row more than the page still needs tells whether another page exists.
		want := limit - len(page.Items) + 1
		rows, err := e.tags.FindTagged(ctx, TagQuery{Tag: input.Tag, View: view, Types: input.Types, After: after, Fields: fields, Limit: want})
		if err != nil {
			return nil, internalErr(err, "list tagged failed")
		}
		for i, row := range rows {
			if len(page.Items) == limit {
				page.NextCursor = encodeTagCursor(lastReturned)
				return page, nil
			}
			item, ok := toTreeSnapshotItem(view, row)
			after = &TagCursor{Weight: item.Tags[input.Tag], ID: row.ID}
			show := ok && input.Filter.matches(item.Tags)
			if show && view == SnapshotViewPublished && row.ParentID != "" {
				if show, err = e.publishedChain(ctx, row, known, visible); err != nil {
					return nil, internalErr(err, "list tagged failed")
				}
			}
			if show {
				page.Items = append(page.Items, item)
				lastReturned = after
			}
			if budget--; budget == 0 {
				if i < len(rows)-1 || len(rows) == want {
					page.NextCursor = encodeTagCursor(after)
				}
				return page, nil
			}
		}
		if len(rows) < want {
			return page, nil
		}
	}
}

func encodeTagCursor(c *TagCursor) string {
	b, _ := json.Marshal(c) // a TagCursor always marshals
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeTagCursor(cursor string) (*TagCursor, error) {
	if cursor == "" {
		return nil, nil
	}
	b, err := base64.RawURLEncoding.DecodeString(cursor)
	var c TagCursor
	if err != nil || json.Unmarshal(b, &c) != nil || c.ID == "" || !isFinite(c.Weight) {
		return nil, badRequest("invalid cursor")
	}
	return &c, nil
}

// GetTagCounts counts how many items visible in view carry each tag, most used first. It
// counts in memory over the snapshot of the requested types, which suits catalogues of up
// to tens of thousands of items; larger ones need an aggregation or a maintained tally.
func (e *Engine) GetTagCounts(ctx context.Context, view SnapshotView, input TagCountsInput) (*TagCounts, error) {
	if _, err := viewLifecycle(view); err != nil {
		return nil, err
	}
	limit := input.Limit
	switch {
	case limit == 0:
		limit = defaultTagCountsLimit
	case limit < 0 || limit > maxChildrenLimit:
		return nil, badRequest(fmt.Sprintf("limit must be between 1 and %d", maxChildrenLimit))
	}
	types := input.Types
	if len(types) == 0 {
		for _, schema := range e.schemas.all() {
			if schema.Tags != nil {
				types = append(types, schema.Type)
			}
		}
		if len(types) == 0 {
			return &TagCounts{Items: []TagCount{}}, nil
		}
	}
	snapshot, err := e.GetSnapshot(ctx, view, SnapshotInput{Types: types})
	if err != nil {
		return nil, err
	}
	counts := map[string]int{}
	for _, item := range snapshot.Items {
		for id := range item.Tags {
			if strings.HasPrefix(id, input.Prefix) {
				counts[id]++
			}
		}
	}
	out := make([]TagCount, 0, len(counts))
	for id, count := range counts {
		out = append(out, TagCount{ID: id, Count: count})
	}
	slices.SortFunc(out, func(a, b TagCount) int {
		return cmp.Or(cmp.Compare(b.Count, a.Count), strings.Compare(a.ID, b.ID))
	})
	return &TagCounts{Items: out[:min(len(out), limit)]}, nil
}
