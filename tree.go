package contentstore

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// ContentTree is the tree API of the engine: items of tree types (see
// ContentTypeSchema.ChildTypes) form a single tree in which every item has at most one
// parent and a name unique within it. *Engine implements it. It is separate from
// ContentStore so existing implementations of that interface keep compiling.
type ContentTree interface {
	Move(ctx context.Context, input MoveInput) (*ContentItem, error)
	ListChildren(ctx context.Context, view SnapshotView, input ListChildrenInput) (*ChildrenPage, error)
	GetTreeItem(ctx context.Context, view SnapshotView, input TreeItemInput) (*TreeItem, error)
}

var _ ContentTree = (*Engine)(nil)

// maxNameLength is the longest tree item name, in bytes.
const maxNameLength = 256

// Move moves and/or renames a tree item. Placement is not versioned: the change applies
// to the draft and published views at once and leaves the draft revision untouched. An
// unchanged placement is a no-op that emits nothing.
func (e *Engine) Move(ctx context.Context, input MoveInput) (*ContentItem, error) {
	if input.Match.ID == "" && input.Match.Key == "" {
		return nil, badRequest("match requires id or key")
	}
	var (
		item  *ContentItem
		moved bool
	)
	err := e.inTreeTransaction(ctx, func(ctx context.Context) error {
		var err error
		moved = false
		item, err = e.loadByMatch(ctx, input.Match)
		if err != nil {
			return err
		}
		if !e.onTree(item) {
			return notInTree(item.Type)
		}
		if err := rejectArchived(item); err != nil {
			return err
		}
		parentID, name := item.ParentID, item.Name
		if input.ParentID != nil {
			parentID = *input.ParentID
		}
		if input.Name != "" {
			name = input.Name
		}
		if parentID == item.ParentID && name == item.Name {
			return nil
		}
		if err := e.checkPlacement(ctx, item, parentID, name); err != nil {
			return err
		}
		item.ParentID, item.Name = parentID, name
		item.UpdatedAt = time.Now().UTC()
		if err := e.store.Upsert(ctx, item, nil); err != nil {
			return internalErr(err, "move failed")
		}
		moved = true
		return nil
	})
	if err != nil {
		return nil, err
	}
	if moved {
		e.emit(item, OpMove)
	}
	return item, nil
}

// onTree reports whether writes to item take the tree path: its type is a tree type, or it
// is already placed (so it stays protected if a schema release drops it from ChildTypes).
func (e *Engine) onTree(item *ContentItem) bool {
	return item.Name != "" || e.schemas.isTreeType(item.Type)
}

func notInTree(t ContentType) *Error {
	return badRequest(fmt.Sprintf("type %q is not part of a content tree", t))
}

// inTreeTransaction runs fn in a tree transaction, which serializes it with every other
// tree write. fn may run more than once, so it must not have effects outside the store.
func (e *Engine) inTreeTransaction(ctx context.Context, fn func(ctx context.Context) error) error {
	if e.tree == nil {
		return internalErr(nil, "store does not support content trees")
	}
	err := e.tree.RunTreeTransaction(ctx, fn)
	if err == nil {
		return nil
	}
	if typed, ok := errors.AsType[*Error](err); ok {
		return typed
	}
	return internalErr(err, "tree transaction failed")
}

// treeWrite re-loads the matched item inside a tree transaction and applies write to it,
// so the write is based on state no other tree write can change before it commits.
func (e *Engine) treeWrite(ctx context.Context, match ContentItemMatch, write func(ctx context.Context, item *ContentItem) error) (*ContentItem, error) {
	var out *ContentItem
	err := e.inTreeTransaction(ctx, func(ctx context.Context) error {
		item, err := e.loadByMatch(ctx, match)
		if err != nil {
			return err
		}
		if err := write(ctx, item); err != nil {
			return err
		}
		out = item
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// saveTreeDraft is SaveDraft for items of a tree type: it resolves, places (on create)
// and writes the item in one tree transaction.
func (e *Engine) saveTreeDraft(ctx context.Context, input SaveDraftInput) (*ContentItem, error) {
	var saved *ContentItem
	err := e.inTreeTransaction(ctx, func(ctx context.Context) error {
		item, created, err := e.resolveForSave(ctx, input)
		if err != nil {
			return err
		}
		if err := e.placeForSave(ctx, item, created, input); err != nil {
			return err
		}
		if err := e.saveDraft(ctx, item, input, input.ExpectedRevision); err != nil {
			return err
		}
		saved = item
		return nil
	})
	if err != nil {
		return nil, err
	}
	e.emit(saved, OpSaveDraft)
	return saved, nil
}

// placeForSave places a newly created tree item, or checks that an update leaves the
// placement as it is.
func (e *Engine) placeForSave(ctx context.Context, item *ContentItem, created bool, input SaveDraftInput) error {
	if !e.onTree(item) {
		return notInTree(item.Type)
	}
	if !created {
		if input.ParentID != "" && input.ParentID != item.ParentID || input.Name != "" && input.Name != item.Name {
			return badRequest("placement cannot change on save: use Move")
		}
		return nil
	}
	if err := e.checkPlacement(ctx, item, input.ParentID, input.Name); err != nil {
		return err
	}
	item.ParentID, item.Name = input.ParentID, input.Name
	return nil
}

// checkPlacement validates placing item under parentID ("" = root level) with name: the
// name is valid and free there, and the parent exists, is not archived, accepts the
// item's type and is not the item itself or one of its descendants.
func (e *Engine) checkPlacement(ctx context.Context, item *ContentItem, parentID, name string) error {
	if err := validateName(name); err != nil {
		return badRequest(err.Error())
	}
	if parentID != "" {
		if parentID == item.ID {
			return badRequest("an item cannot be its own parent")
		}
		parent, err := e.store.GetByID(ctx, parentID)
		if errors.Is(err, ErrNotFound) {
			return badRequest(fmt.Sprintf("parent %q not found", parentID))
		}
		if err != nil {
			return internalErr(err, "load parent failed")
		}
		if parent.ArchivedAt != nil {
			return notAcceptable("parent is archived: unarchive it first")
		}
		if !e.schemas.accepts(parent.Type, item.Type) {
			return badRequest(fmt.Sprintf("type %q does not accept items of type %q", parent.Type, item.Type))
		}
		ancestors, err := e.ancestors(ctx, parent)
		if err != nil {
			return err
		}
		for _, ancestor := range ancestors {
			if ancestor.ID == item.ID {
				return badRequest("an item cannot be moved into its own subtree")
			}
		}
	}
	siblings, err := e.tree.FindChildren(ctx, ChildQuery{ParentID: parentID, Name: name, Fields: []FieldID{}, Limit: 2})
	if err != nil {
		return internalErr(err, "load siblings failed")
	}
	for _, sibling := range siblings {
		if sibling.ID != item.ID {
			return conflict(fmt.Sprintf("name %q is already taken in this parent", name))
		}
	}
	return nil
}

// ancestors returns the items above item, nearest first. A missing parent or a revisited
// item means the stored tree is corrupt.
func (e *Engine) ancestors(ctx context.Context, item *ContentItem) ([]*ContentItem, error) {
	var out []*ContentItem
	seen := map[string]bool{item.ID: true}
	for id := item.ParentID; id != ""; {
		if seen[id] {
			return nil, internalErr(nil, fmt.Sprintf("content tree contains a cycle at %q", id))
		}
		seen[id] = true
		parent, err := e.store.GetByID(ctx, id)
		if err != nil {
			return nil, internalErr(err, fmt.Sprintf("load tree parent %q failed", id))
		}
		out = append(out, parent)
		id = parent.ParentID
	}
	return out, nil
}

// rejectChildren fails when item has children in the given lifecycle states.
func (e *Engine) rejectChildren(ctx context.Context, item *ContentItem, lifecycle ChildLifecycle, hint string) error {
	children, err := e.tree.FindChildren(ctx, ChildQuery{ParentID: item.ID, Lifecycle: lifecycle, Fields: []FieldID{}, Limit: 1})
	if err != nil {
		return internalErr(err, "load children failed")
	}
	if len(children) > 0 {
		return notAcceptable("item has children: " + hint)
	}
	return nil
}

// rejectArchivedParent keeps a non-archived item from having an archived parent.
func (e *Engine) rejectArchivedParent(ctx context.Context, item *ContentItem) error {
	if item.ParentID == "" {
		return nil
	}
	parent, err := e.store.GetByID(ctx, item.ParentID)
	if err != nil {
		return internalErr(err, fmt.Sprintf("load tree parent %q failed", item.ParentID))
	}
	if parent.ArchivedAt != nil {
		return notAcceptable("parent is archived: unarchive it first")
	}
	return nil
}

// deleteTreeItem is Delete for tree items: it re-resolves the item and checks it has no
// children inside a tree transaction.
func (e *Engine) deleteTreeItem(ctx context.Context, match ContentItemMatch) (bool, error) {
	var deleted *ContentItem
	err := e.inTreeTransaction(ctx, func(ctx context.Context) error {
		deleted = nil
		item, err := e.resolveOptional(ctx, match)
		if err != nil || item == nil {
			return err
		}
		if err := e.rejectChildren(ctx, item, ChildrenAny, "delete or move its children first"); err != nil {
			return err
		}
		if err := e.store.Delete(ctx, item.ID); err != nil {
			return internalErr(err, "delete failed")
		}
		deleted = item
		return nil
	})
	if err != nil || deleted == nil {
		return false, err
	}
	e.emit(deleted, OpDelete)
	return true, nil
}

// validateName checks a tree item name: it is a single path segment.
func validateName(name string) error {
	switch {
	case name == "":
		return errors.New("name is required for items of a tree type")
	case len(name) > maxNameLength:
		return fmt.Errorf("name exceeds %d bytes", maxNameLength)
	case !utf8.ValidString(name):
		return errors.New("name must be valid UTF-8")
	case name == "." || name == "..":
		return fmt.Errorf("name %q is reserved", name)
	case strings.ContainsRune(name, '/'):
		return errors.New("name must not contain '/'")
	case strings.ContainsFunc(name, unicode.IsControl):
		return errors.New("name must not contain control characters")
	}
	return nil
}
