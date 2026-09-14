package contentstore

import (
	"context"
	"testing"
	"time"
)

func TestExportContentIncludesEveryLifecycleState(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	now := time.Date(2026, time.September, 11, 8, 0, 0, 0, time.UTC)
	archivedAt := now.Add(time.Hour)
	store.items["item-b"] = transferTestItem("item-b", "key-b", now)
	store.items["item-b"].ArchivedAt = &archivedAt
	store.items["item-a"] = transferTestItem("item-a", "key-a", now)
	store.items["item-a"].Published = cloneContentItem(store.items["item-a"]).Draft

	export, err := newTestEngine(store).ExportContent(context.Background())
	if err != nil {
		t.Fatalf("ExportContent: %v", err)
	}
	if export.FormatVersion != ContentExportFormatVersion || export.ItemCount != 2 {
		t.Fatalf("unexpected export metadata: %#v", export)
	}
	if export.Items[0].ID != "item-a" || export.Items[1].ID != "item-b" {
		t.Fatalf("items are not sorted by id: %#v", export.Items)
	}
	if export.Items[0].Published == nil || export.Items[1].ArchivedAt == nil {
		t.Fatalf("export lost lifecycle data: %#v", export.Items)
	}
	wantChecksum, err := contentItemsChecksum(export.Items)
	if err != nil {
		t.Fatalf("contentItemsChecksum: %v", err)
	}
	if export.Checksum != wantChecksum {
		t.Fatalf("checksum = %q, want %q", export.Checksum, wantChecksum)
	}
}

func TestReplaceContentIsForbiddenOnCanonical(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	store.items["old"] = transferTestItem("old", "old-key", time.Now().UTC())
	export, err := newContentExport([]*ContentItem{transferTestItem("new", "new-key", time.Now().UTC())})
	if err != nil {
		t.Fatalf("newContentExport: %v", err)
	}

	_, err = newTestEngine(store).ReplaceContent(context.Background(), *export)
	wantKind(t, err, KindForbidden)
	if _, ok := store.items["old"]; !ok || len(store.items) != 1 {
		t.Fatalf("canonical data changed after rejected replacement: %#v", store.items)
	}
}

func TestExportContentIsForbiddenOnSandbox(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	store.items["item"] = transferTestItem("item", "item-key", time.Now().UTC())
	engine := New(Deps{Store: store, Schemas: testRegistry(), Locales: testLocales, NewID: testNewID(), Role: DeploymentRoleSandbox})

	_, err := engine.ExportContent(context.Background())
	wantKind(t, err, KindForbidden)
}

func TestReplaceContentRejectsInvalidExportBeforeWrite(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	tests := []struct {
		name   string
		mutate func(*ContentExport)
	}{
		{name: "format version", mutate: func(export *ContentExport) { export.FormatVersion++ }},
		{name: "item count", mutate: func(export *ContentExport) { export.ItemCount++ }},
		{name: "checksum", mutate: func(export *ContentExport) { export.Checksum = "sha256:wrong" }},
		{name: "duplicate id", mutate: func(export *ContentExport) {
			export.Items = append(export.Items, transferTestItem("new", "another-key", now))
			export.ItemCount++
		}},
		{name: "duplicate key", mutate: func(export *ContentExport) {
			export.Items = append(export.Items, transferTestItem("another-id", "new-key", now))
			export.ItemCount++
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			store := newFakeStore()
			store.items["old"] = transferTestItem("old", "old-key", now)
			export, err := newContentExport([]*ContentItem{transferTestItem("new", "new-key", now)})
			if err != nil {
				t.Fatalf("newContentExport: %v", err)
			}
			tt.mutate(export)
			engine := New(Deps{Store: store, Schemas: testRegistry(), Locales: testLocales, NewID: testNewID(), Role: DeploymentRoleSandbox})

			_, err = engine.ReplaceContent(context.Background(), *export)
			wantKind(t, err, KindBadRequest)
			if _, ok := store.items["old"]; !ok || len(store.items) != 1 {
				t.Fatalf("data changed after rejected replacement: %#v", store.items)
			}
		})
	}
}

func TestReplaceContentInstallsSnapshotAndEmitsReset(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	store.items["old"] = transferTestItem("old", "old-key", time.Now().UTC())
	notifier := &recordingNotifier{}
	engine := New(Deps{
		Store:   store,
		Schemas: testRegistry(),
		Locales: testLocales,
		NewID:   testNewID(),
		Notify:  notifier.record,
		Role:    DeploymentRoleSandbox,
	})
	export, err := newContentExport([]*ContentItem{
		transferTestItem("new-b", "new-key-b", time.Now().UTC()),
		transferTestItem("new-a", "new-key-a", time.Now().UTC()),
	})
	if err != nil {
		t.Fatalf("newContentExport: %v", err)
	}

	result, err := engine.ReplaceContent(context.Background(), *export)
	if err != nil {
		t.Fatalf("ReplaceContent: %v", err)
	}
	if result.ItemCount != export.ItemCount || result.Checksum != export.Checksum {
		t.Fatalf("result = %#v, want count=%d checksum=%q", result, export.ItemCount, export.Checksum)
	}
	if _, oldExists := store.items["old"]; oldExists || len(store.items) != 2 {
		t.Fatalf("replacement was not exact: %#v", store.items)
	}
	if len(notifier.changes) != 1 || notifier.changes[0].Op != OpContentReset {
		t.Fatalf("notifications = %#v, want one content reset", notifier.changes)
	}
}

func TestParseDeploymentRole(t *testing.T) {
	t.Parallel()
	for _, value := range []string{string(DeploymentRoleCanonical), string(DeploymentRoleSandbox)} {
		if role, err := ParseDeploymentRole(value); err != nil || string(role) != value {
			t.Errorf("ParseDeploymentRole(%q) = %q, %v", value, role, err)
		}
	}
	for _, value := range []string{"", "production", "Sandbox"} {
		if _, err := ParseDeploymentRole(value); err == nil {
			t.Errorf("ParseDeploymentRole(%q) succeeded", value)
		}
	}
}

func transferTestItem(id, key string, now time.Time) *ContentItem {
	return &ContentItem{
		ID:        id,
		Key:       key,
		Type:      testType,
		CreatedAt: now,
		UpdatedAt: now,
		Draft: &ContentSnapshot{
			Revision:      3,
			SchemaVersion: "1",
			SchemaHash:    "schema-hash",
			Fields:        validFields(),
			CreatedAt:     now,
			UpdatedAt:     now,
		},
	}
}
