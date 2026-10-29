package contentstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

// ExportContent returns every stored document, including archived items and
// both lifecycle snapshots, in a deterministic checksummed envelope.
func (e *Engine) ExportContent(ctx context.Context) (*ContentExport, error) {
	if e.role != DeploymentRoleCanonical {
		return nil, forbidden("content export is allowed only on a canonical deployment")
	}
	items, err := e.store.ExportAll(ctx)
	if err != nil {
		return nil, internalErr(err, "content export failed")
	}
	export, err := newContentExport(items)
	if err != nil {
		return nil, internalErr(err, "content export checksum failed")
	}
	return export, nil
}

// ReplaceContent atomically replaces all data on a sandbox instance. Canonical
// instances reject the operation before inspecting or writing the payload.
func (e *Engine) ReplaceContent(ctx context.Context, export ContentExport) (*ReplaceContentResult, error) {
	if e.role != DeploymentRoleSandbox {
		return nil, forbidden("content replacement is allowed only on a sandbox deployment")
	}
	if err := validateContentExport(export); err != nil {
		return nil, badRequest(err.Error())
	}
	if err := e.store.ReplaceAll(ctx, export.Items); err != nil {
		return nil, internalErr(err, "content replacement failed")
	}
	if e.notify != nil {
		e.notify(ContentChange{Op: OpContentReset})
	}
	return &ReplaceContentResult{ItemCount: export.ItemCount, Checksum: export.Checksum}, nil
}

func newContentExport(items []*ContentItem) (*ContentExport, error) {
	if items == nil {
		items = []*ContentItem{}
	}
	sorted := append([]*ContentItem(nil), items...)
	if err := validateContentItems(sorted); err != nil {
		return nil, err
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	checksum, err := contentItemsChecksum(sorted)
	if err != nil {
		return nil, err
	}
	return &ContentExport{
		FormatVersion: ContentExportFormatVersion,
		CreatedAt:     time.Now().UTC(),
		ItemCount:     len(sorted),
		Checksum:      checksum,
		Items:         sorted,
	}, nil
}

func validateContentExport(export ContentExport) error {
	if export.FormatVersion != ContentExportFormatVersion {
		return fmt.Errorf("unsupported content export format version %d", export.FormatVersion)
	}
	if export.ItemCount != len(export.Items) {
		return fmt.Errorf("content export item count is %d, payload contains %d items", export.ItemCount, len(export.Items))
	}
	if err := validateContentItems(export.Items); err != nil {
		return err
	}
	if err := validateContentTree(export.Items); err != nil {
		return err
	}
	checksum, err := contentItemsChecksum(export.Items)
	if err != nil {
		return fmt.Errorf("calculate content export checksum: %w", err)
	}
	if checksum != export.Checksum {
		return fmt.Errorf("content export checksum mismatch")
	}
	return nil
}

func validateContentItems(items []*ContentItem) error {
	ids := make(map[string]struct{}, len(items))
	keys := make(map[string]struct{}, len(items))
	for index, item := range items {
		if item == nil {
			return fmt.Errorf("content export item %d is null", index)
		}
		if item.ID == "" {
			return fmt.Errorf("content export item %d has no id", index)
		}
		if item.Type == "" {
			return fmt.Errorf("content export item %q has no type", item.ID)
		}
		if _, exists := ids[item.ID]; exists {
			return fmt.Errorf("content export contains duplicate id %q", item.ID)
		}
		ids[item.ID] = struct{}{}
		if item.Key == "" {
			continue
		}
		if _, exists := keys[item.Key]; exists {
			return fmt.Errorf("content export contains duplicate key %q", item.Key)
		}
		keys[item.Key] = struct{}{}
	}
	return nil
}

// validateContentTree checks the tree invariants an import must satisfy: placed items have
// a valid name unique within their parent, every parent is in the payload, there are no
// cycles, and no live item sits under an archived parent. Without a schema registry it
// cannot check ChildTypes. Exports deliberately skip it so a backup always succeeds.
func validateContentTree(items []*ContentItem) error {
	byID := make(map[string]*ContentItem, len(items))
	for _, item := range items {
		byID[item.ID] = item
	}
	names := map[[2]string]string{}
	for _, item := range items {
		if item.Name == "" {
			if item.ParentID != "" {
				return fmt.Errorf("content export item %q has a parent but no name", item.ID)
			}
			continue
		}
		if err := validateName(item.Name); err != nil {
			return fmt.Errorf("content export item %q: %w", item.ID, err)
		}
		placement := [2]string{item.ParentID, item.Name}
		if other, exists := names[placement]; exists {
			return fmt.Errorf("content export items %q and %q have the same name %q in one parent", other, item.ID, item.Name)
		}
		names[placement] = item.ID
		if item.ParentID == "" {
			continue
		}
		parent, exists := byID[item.ParentID]
		if !exists {
			return fmt.Errorf("content export item %q has a missing parent %q", item.ID, item.ParentID)
		}
		if item.ArchivedAt == nil && parent.ArchivedAt != nil {
			return fmt.Errorf("content export item %q is not archived but its parent %q is", item.ID, parent.ID)
		}
	}
	// Every parent exists now, so walking up from each item ends at the root or a cycle.
	acyclic := map[string]bool{}
	for _, item := range items {
		seen := map[string]bool{}
		for id := item.ID; id != "" && !acyclic[id]; id = byID[id].ParentID {
			if seen[id] {
				return fmt.Errorf("content export tree contains a cycle at %q", id)
			}
			seen[id] = true
		}
		for id := range seen {
			acyclic[id] = true
		}
	}
	return nil
}

func contentItemsChecksum(items []*ContentItem) (string, error) {
	sorted := append([]*ContentItem(nil), items...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	payload, err := json.Marshal(sorted)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}
