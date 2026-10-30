package contentstore

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"strings"
)

const (
	defaultChildrenLimit = 100
	maxChildrenLimit     = 1000
)

// ListChildren returns one page of the children of a tree item (or of the named root-level
// items) visible in view, ordered by name. In the published view an item is visible while
// it and all its ancestors are published and not archived; in the preview view while none
// of them is archived. A parent that is not visible is reported as not found.
func (e *Engine) ListChildren(ctx context.Context, view SnapshotView, input ListChildrenInput) (*ChildrenPage, error) {
	lifecycle, err := childLifecycle(view)
	if err != nil {
		return nil, err
	}
	limit := input.Limit
	switch {
	case limit == 0:
		limit = defaultChildrenLimit
	case limit < 0 || limit > maxChildrenLimit:
		return nil, badRequest(fmt.Sprintf("limit must be between 1 and %d", maxChildrenLimit))
	}
	afterName, err := decodeCursor(input.Cursor)
	if err != nil {
		return nil, err
	}
	fields, err := e.projection(input.Types, input.Fields)
	if err != nil {
		return nil, err
	}
	if e.tree == nil {
		return nil, internalErr(nil, "store does not support content trees")
	}
	if input.ParentID != "" {
		parent, err := e.loadByMatch(ctx, ContentItemMatch{ID: input.ParentID})
		if err != nil {
			return nil, err
		}
		if _, err := e.visibleAncestors(ctx, view, parent); err != nil {
			return nil, err
		}
	}

	children, err := e.tree.FindChildren(ctx, ChildQuery{
		ParentID:  input.ParentID,
		AfterName: afterName,
		Lifecycle: lifecycle,
		Types:     input.Types,
		Fields:    fields,
		Limit:     limit + 1,
	})
	if err != nil {
		return nil, internalErr(err, "list children failed")
	}
	page := &ChildrenPage{Items: make([]SnapshotItem, 0, min(len(children), limit))}
	if len(children) > limit {
		children = children[:limit]
		page.NextCursor = base64.RawURLEncoding.EncodeToString([]byte(children[limit-1].Name))
	}
	for _, child := range children {
		if item, ok := toTreeSnapshotItem(view, child); ok {
			page.Items = append(page.Items, item)
		}
	}
	return page, nil
}

// GetTreeItem returns one item visible in view, addressed by match or by path, with its
// ancestors and path. An item that is not visible (see ListChildren) is not found.
func (e *Engine) GetTreeItem(ctx context.Context, view SnapshotView, input TreeItemInput) (*TreeItem, error) {
	lifecycle, err := childLifecycle(view)
	if err != nil {
		return nil, err
	}
	byMatch := input.Match.ID != "" || input.Match.Key != ""
	if byMatch == (input.Path != "") {
		return nil, badRequest("exactly one of match or path is required")
	}
	if byMatch {
		item, err := e.loadByMatch(ctx, input.Match)
		if err != nil {
			return nil, err
		}
		fields, err := e.projection([]ContentType{item.Type}, input.Fields)
		if err != nil {
			return nil, err
		}
		ancestors, err := e.visibleAncestors(ctx, view, item)
		if err != nil {
			return nil, err
		}
		slices.Reverse(ancestors)
		return newTreeItem(view, projectFields(item, fields), ancestors)
	}

	segments, err := splitPath(input.Path)
	if err != nil {
		return nil, err
	}
	fields, err := e.projection(nil, input.Fields)
	if err != nil {
		return nil, err
	}
	if e.tree == nil {
		return nil, internalErr(nil, "store does not support content trees")
	}
	var (
		parentID  string
		ancestors []*ContentItem
		item      *ContentItem
	)
	for i, name := range segments {
		segmentFields := []FieldID{}
		if i == len(segments)-1 {
			segmentFields = fields
		}
		found, err := e.tree.FindChildren(ctx, ChildQuery{ParentID: parentID, Name: name, Lifecycle: lifecycle, Fields: segmentFields, Limit: 1})
		if err != nil {
			return nil, internalErr(err, "resolve path failed")
		}
		if len(found) == 0 {
			return nil, notFound(fmt.Sprintf("path %q not found", input.Path))
		}
		if item != nil {
			ancestors = append(ancestors, item)
		}
		item = found[0]
		parentID = item.ID
	}
	return newTreeItem(view, item, ancestors)
}

func newTreeItem(view SnapshotView, item *ContentItem, ancestors []*ContentItem) (*TreeItem, error) {
	snapshotItem, ok := toTreeSnapshotItem(view, item)
	if !ok {
		return nil, notFound("item not found")
	}
	out := &TreeItem{Item: snapshotItem, Ancestors: make([]TreeRef, 0, len(ancestors))}
	segments := make([]string, 0, len(ancestors)+1)
	for _, ancestor := range ancestors {
		out.Ancestors = append(out.Ancestors, TreeRef{ID: ancestor.ID, Type: ancestor.Type, Name: ancestor.Name})
		segments = append(segments, ancestor.Name)
	}
	if item.Name != "" {
		out.Path = "/" + strings.Join(append(segments, item.Name), "/")
	}
	return out, nil
}

// toTreeSnapshotItem is toSnapshotItem for tree reads, which return an empty field set
// rather than null when a projection leaves no fields.
func toTreeSnapshotItem(view SnapshotView, item *ContentItem) (SnapshotItem, bool) {
	out, ok := toSnapshotItem(view, item)
	if ok && out.Fields == nil {
		out.Fields = ContentFields{}
	}
	return out, ok
}

// visibleAncestors returns item's ancestors (nearest first) when item and all of them are
// visible in view, and a not-found error otherwise.
func (e *Engine) visibleAncestors(ctx context.Context, view SnapshotView, item *ContentItem) ([]*ContentItem, error) {
	if !visibleIn(view, item) {
		return nil, notFound("item not found")
	}
	ancestors, err := e.ancestors(ctx, item)
	if err != nil {
		return nil, err
	}
	for _, ancestor := range ancestors {
		if !visibleIn(view, ancestor) {
			return nil, notFound("item not found")
		}
	}
	return ancestors, nil
}

// visibleIn reports whether item itself (ignoring its ancestors) is visible in view.
func visibleIn(view SnapshotView, item *ContentItem) bool {
	if item.ArchivedAt != nil {
		return false
	}
	return view != SnapshotViewPublished || item.Published != nil
}

func childLifecycle(view SnapshotView) (ChildLifecycle, error) {
	switch view {
	case SnapshotViewPublished:
		return ChildrenPublished, nil
	case SnapshotViewPreview:
		return ChildrenLive, nil
	default:
		return 0, badRequest(fmt.Sprintf("unsupported snapshot view %q", view))
	}
}

// withPublishedAncestors drops placed items with an ancestor that is not published or is
// archived (items come from FindPublished, so they are visible themselves). Ancestors in
// items are reused; others are loaded by ID, once per snapshot. Snapshots without placed
// items are returned untouched without any further reads.
func (e *Engine) withPublishedAncestors(ctx context.Context, items []*ContentItem) ([]*ContentItem, error) {
	if !slices.ContainsFunc(items, func(item *ContentItem) bool { return item.ParentID != "" }) {
		return items, nil
	}
	known := make(map[string]*ContentItem, len(items))
	for _, item := range items {
		known[item.ID] = item
	}
	visible := map[string]bool{} // ID -> it and all its ancestors are visible
	out := make([]*ContentItem, 0, len(items))
	for _, item := range items {
		ok, err := e.publishedChain(ctx, item, known, visible)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, item)
		}
	}
	return out, nil
}

// publishedChain reports whether item and all its ancestors are visible in the published
// view, loading unknown ancestors into known and memoizing every answer in visible. A
// missing ancestor or a cycle makes the chain invisible.
func (e *Engine) publishedChain(ctx context.Context, item *ContentItem, known map[string]*ContentItem, visible map[string]bool) (bool, error) {
	var chain []string
	result := false
	for {
		if memo, ok := visible[item.ID]; ok {
			result = memo
			break
		}
		if !visibleIn(SnapshotViewPublished, item) || slices.Contains(chain, item.ID) {
			break
		}
		chain = append(chain, item.ID)
		if item.ParentID == "" {
			result = true
			break
		}
		parent, ok := known[item.ParentID]
		if !ok {
			loaded, err := e.store.GetByID(ctx, item.ParentID)
			if errors.Is(err, ErrNotFound) {
				break
			}
			if err != nil {
				return false, err
			}
			known[loaded.ID] = loaded
			parent = loaded
		}
		item = parent
	}
	for _, id := range chain {
		visible[id] = result
	}
	return result, nil
}

// projection validates requested field IDs and returns them in the form ChildQuery takes:
// nil (all fields) when none were requested. Each must belong to one of types, or to any
// registered schema when types is empty.
func (e *Engine) projection(types []ContentType, fields []FieldID) ([]FieldID, error) {
	if len(fields) == 0 {
		return nil, nil
	}
	if len(types) == 0 {
		for _, schema := range e.schemas.all() {
			types = append(types, schema.Type)
		}
	}
	for _, field := range fields {
		known := slices.ContainsFunc(types, func(t ContentType) bool {
			schema, ok := e.schemas.schemaFor(t)
			if !ok {
				return false
			}
			_, ok = schema.field(field)
			return ok
		})
		if !known {
			return nil, badRequest(fmt.Sprintf("unknown field %q", field))
		}
	}
	return fields, nil
}

// projectFields returns a copy of item whose snapshots keep only the given fields (all
// when fields is nil).
func projectFields(item *ContentItem, fields []FieldID) *ContentItem {
	if fields == nil {
		return item
	}
	out := *item
	project := func(snapshot *ContentSnapshot) *ContentSnapshot {
		if snapshot == nil {
			return nil
		}
		projected := *snapshot
		projected.Fields = ContentFields{}
		for _, field := range fields {
			if values, ok := snapshot.Fields[string(field)]; ok {
				projected.Fields[string(field)] = values
			}
		}
		return &projected
	}
	out.Draft = project(item.Draft)
	out.Published = project(item.Published)
	return &out
}

// splitPath splits "/a/b/c" (leading slash optional) into names, rejecting empty or
// invalid segments.
func splitPath(path string) ([]string, error) {
	segments := strings.Split(strings.TrimPrefix(path, "/"), "/")
	for _, segment := range segments {
		if err := validateName(segment); err != nil {
			return nil, badRequest(fmt.Sprintf("invalid path %q: %v", path, err))
		}
	}
	return segments, nil
}

func decodeCursor(cursor string) (string, error) {
	if cursor == "" {
		return "", nil
	}
	name, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil || len(name) == 0 {
		return "", badRequest("invalid cursor")
	}
	return string(name), nil
}
