package contentstore

import (
	"context"
	"slices"
	"testing"
)

// readTree builds:
//
//	/kb            folder, published
//	/kb/a          document, published
//	/kb/b          document, draft only
//	/kb/c          document, archived
//	/kb/drafts     folder, draft only
//	/kb/drafts/d   document, published (hidden from the published view by its parent)
//	/readme        document, published
func readTree(t *testing.T) (*Engine, map[string]*ContentItem) {
	t.Helper()
	e := newTreeEngine(newFakeStore())
	return e, buildReadTree(t, e)
}

func buildReadTree(t *testing.T, e *Engine) map[string]*ContentItem {
	t.Helper()
	ctx := context.Background()
	items := map[string]*ContentItem{}
	items["kb"] = place(t, e, folderType, "", "kb")
	items["a"] = place(t, e, documentType, items["kb"].ID, "a")
	items["b"] = place(t, e, documentType, items["kb"].ID, "b")
	items["c"] = place(t, e, documentType, items["kb"].ID, "c")
	items["drafts"] = place(t, e, folderType, items["kb"].ID, "drafts")
	items["d"] = place(t, e, documentType, items["drafts"].ID, "d")
	items["readme"] = place(t, e, documentType, "", "readme")
	if _, err := e.SetFieldValue(ctx, SetFieldValueInput{Match: ContentItemMatch{ID: items["a"].ID}, FieldID: "body", Locale: "de", Value: "# A"}); err != nil {
		t.Fatalf("body: %v", err)
	}
	for _, name := range []string{"kb", "a", "d", "readme"} {
		if _, err := e.Publish(ctx, ContentItemMatch{ID: items[name].ID}); err != nil {
			t.Fatalf("publish %s: %v", name, err)
		}
	}
	if _, err := e.Archive(ctx, ContentItemMatch{ID: items["c"].ID}); err != nil {
		t.Fatalf("archive: %v", err)
	}
	return items
}

func names(items []SnapshotItem) []string {
	out := make([]string, 0, len(items))
	for _, item := range items {
		out = append(out, item.Name)
	}
	return out
}

func TestListChildren(t *testing.T) {
	ctx := context.Background()
	e, items := readTree(t)
	kb := items["kb"].ID

	tests := []struct {
		name  string
		view  SnapshotView
		input ListChildrenInput
		want  []string
	}{
		{"preview", SnapshotViewPreview, ListChildrenInput{ParentID: kb}, []string{"a", "b", "drafts"}},
		{"published", SnapshotViewPublished, ListChildrenInput{ParentID: kb}, []string{"a"}},
		{"by type", SnapshotViewPreview, ListChildrenInput{ParentID: kb, Types: []ContentType{folderType}}, []string{"drafts"}},
		{"root preview", SnapshotViewPreview, ListChildrenInput{}, []string{"kb", "readme"}},
		{"root published", SnapshotViewPublished, ListChildrenInput{}, []string{"kb", "readme"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			page, err := e.ListChildren(ctx, tt.view, tt.input)
			if err != nil {
				t.Fatalf("list: %v", err)
			}
			if got := names(page.Items); !slices.Equal(got, tt.want) || page.NextCursor != "" {
				t.Fatalf("got %v (cursor %q), want %v", got, page.NextCursor, tt.want)
			}
		})
	}

	page, err := e.ListChildren(ctx, SnapshotViewPreview, ListChildrenInput{ParentID: kb})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if a := page.Items[0]; a.ParentID != kb || a.State != StatePublished || a.Fields["body"]["de"] != "# A" {
		t.Fatalf("child a = %+v", a)
	}
	if b := page.Items[1]; b.State != StateDraft {
		t.Fatalf("child b state = %s", b.State)
	}
}

func TestListChildrenProjectsFields(t *testing.T) {
	ctx := context.Background()
	e, items := readTree(t)
	page, err := e.ListChildren(ctx, SnapshotViewPreview, ListChildrenInput{ParentID: items["kb"].ID, Fields: []FieldID{"title"}})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, item := range page.Items {
		if _, ok := item.Fields["body"]; ok || item.Fields["title"]["de"] != item.Name {
			t.Fatalf("projected fields of %s = %v", item.Name, item.Fields)
		}
	}
	// State still derives from the revisions when fields are projected away.
	if page.Items[0].State != StatePublished {
		t.Fatalf("projected state = %s", page.Items[0].State)
	}
	page, err = e.ListChildren(ctx, SnapshotViewPreview, ListChildrenInput{ParentID: items["kb"].ID, Fields: []FieldID{"frontMatter"}})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if page.Items[0].Fields == nil || len(page.Items[0].Fields) != 0 {
		t.Fatalf("a projection without values must give an empty field set, got %#v", page.Items[0].Fields)
	}
}

func TestListChildrenPaginates(t *testing.T) {
	ctx := context.Background()
	e, items := readTree(t)
	input := ListChildrenInput{ParentID: items["kb"].ID, Limit: 2}
	var got []string
	for range 3 {
		page, err := e.ListChildren(ctx, SnapshotViewPreview, input)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		got = append(got, names(page.Items)...)
		if page.NextCursor == "" {
			break
		}
		input.Cursor = page.NextCursor
	}
	if want := []string{"a", "b", "drafts"}; !slices.Equal(got, want) {
		t.Fatalf("paged names = %v, want %v", got, want)
	}
}

func TestListChildrenRejects(t *testing.T) {
	ctx := context.Background()
	e, items := readTree(t)
	tests := []struct {
		name  string
		view  SnapshotView
		input ListChildrenInput
		kind  Kind
	}{
		{"unknown view", "nope", ListChildrenInput{}, KindBadRequest},
		{"negative limit", SnapshotViewPreview, ListChildrenInput{Limit: -1}, KindBadRequest},
		{"limit above maximum", SnapshotViewPreview, ListChildrenInput{Limit: 1001}, KindBadRequest},
		{"invalid cursor", SnapshotViewPreview, ListChildrenInput{Cursor: "!"}, KindBadRequest},
		{"unknown field", SnapshotViewPreview, ListChildrenInput{Fields: []FieldID{"nope"}}, KindBadRequest},
		{"field of another type", SnapshotViewPreview, ListChildrenInput{Types: []ContentType{folderType}, Fields: []FieldID{"body"}}, KindBadRequest},
		{"missing parent", SnapshotViewPreview, ListChildrenInput{ParentID: "nope"}, KindNotFound},
		{"unpublished parent", SnapshotViewPublished, ListChildrenInput{ParentID: items["drafts"].ID}, KindNotFound},
		{"archived parent", SnapshotViewPreview, ListChildrenInput{ParentID: items["c"].ID}, KindNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := e.ListChildren(ctx, tt.view, tt.input)
			wantKind(t, err, tt.kind)
		})
	}
}

func TestGetTreeItem(t *testing.T) {
	ctx := context.Background()
	e, items := readTree(t)

	for _, input := range []TreeItemInput{
		{Path: "/kb/drafts/d"},
		{Path: "kb/drafts/d"},
		{Match: ContentItemMatch{ID: items["d"].ID}},
	} {
		got, err := e.GetTreeItem(ctx, SnapshotViewPreview, input)
		if err != nil {
			t.Fatalf("%+v: %v", input, err)
		}
		if got.Item.ID != items["d"].ID || got.Path != "/kb/drafts/d" || len(got.Ancestors) != 2 ||
			got.Ancestors[0].ID != items["kb"].ID || got.Ancestors[1] != (TreeRef{ID: items["drafts"].ID, Type: folderType, Name: "drafts"}) {
			t.Fatalf("%+v: got %+v", input, got)
		}
	}

	got, err := e.GetTreeItem(ctx, SnapshotViewPublished, TreeItemInput{Path: "/kb/a", Fields: []FieldID{"title"}})
	if err != nil {
		t.Fatalf("published path: %v", err)
	}
	if _, ok := got.Item.Fields["body"]; ok || got.Item.Fields["title"]["de"] != "a" || got.Item.State != StatePublished {
		t.Fatalf("projected published item = %+v", got.Item)
	}
	got, err = e.GetTreeItem(ctx, SnapshotViewPublished, TreeItemInput{Match: ContentItemMatch{ID: items["a"].ID}, Fields: []FieldID{"title"}})
	if err != nil || len(got.Item.Fields) != 1 {
		t.Fatalf("projected by match: %+v, %v", got, err)
	}

	legacy, err := e.SaveDraft(ctx, SaveDraftInput{Type: testType, Fields: validFields()})
	if err != nil {
		t.Fatalf("legacy: %v", err)
	}
	got, err = e.GetTreeItem(ctx, SnapshotViewPreview, TreeItemInput{Match: ContentItemMatch{ID: legacy.ID}})
	if err != nil || got.Path != "" || len(got.Ancestors) != 0 || got.Ancestors == nil {
		t.Fatalf("non-tree item: %+v, %v", got, err)
	}
}

func TestGetTreeItemRejects(t *testing.T) {
	ctx := context.Background()
	e, items := readTree(t)
	tests := []struct {
		name  string
		view  SnapshotView
		input TreeItemInput
		kind  Kind
	}{
		{"neither match nor path", SnapshotViewPreview, TreeItemInput{}, KindBadRequest},
		{"both match and path", SnapshotViewPreview, TreeItemInput{Match: ContentItemMatch{ID: items["a"].ID}, Path: "/kb/a"}, KindBadRequest},
		{"root path", SnapshotViewPreview, TreeItemInput{Path: "/"}, KindBadRequest},
		{"empty segment", SnapshotViewPreview, TreeItemInput{Path: "/kb//a"}, KindBadRequest},
		{"trailing slash", SnapshotViewPreview, TreeItemInput{Path: "/kb/a/"}, KindBadRequest},
		{"dot segment", SnapshotViewPreview, TreeItemInput{Path: "/kb/../a"}, KindBadRequest},
		{"missing path", SnapshotViewPreview, TreeItemInput{Path: "/kb/nope"}, KindNotFound},
		{"unknown field", SnapshotViewPreview, TreeItemInput{Path: "/kb/a", Fields: []FieldID{"nope"}}, KindBadRequest},
		{"archived item", SnapshotViewPreview, TreeItemInput{Path: "/kb/c"}, KindNotFound},
		{"draft item in published view", SnapshotViewPublished, TreeItemInput{Path: "/kb/b"}, KindNotFound},
		{"published item under a draft folder, by path", SnapshotViewPublished, TreeItemInput{Path: "/kb/drafts/d"}, KindNotFound},
		{"published item under a draft folder, by match", SnapshotViewPublished, TreeItemInput{Match: ContentItemMatch{ID: items["d"].ID}}, KindNotFound},
		{"archived item by match", SnapshotViewPreview, TreeItemInput{Match: ContentItemMatch{ID: items["c"].ID}}, KindNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := e.GetTreeItem(ctx, tt.view, tt.input)
			wantKind(t, err, tt.kind)
		})
	}
}

func TestGetSnapshotHidesItemsUnderUnpublishedAncestors(t *testing.T) {
	ctx := context.Background()
	e, items := readTree(t)
	ids := func(view SnapshotView) []string {
		snapshot, err := e.GetSnapshot(ctx, view, SnapshotInput{Types: []ContentType{documentType}})
		if err != nil {
			t.Fatalf("snapshot: %v", err)
		}
		var out []string
		for _, item := range snapshot.Items {
			out = append(out, item.Name)
		}
		slices.Sort(out)
		return out
	}
	if got, want := ids(SnapshotViewPublished), []string{"a", "readme"}; !slices.Equal(got, want) {
		t.Fatalf("published = %v, want %v", got, want)
	}
	if got, want := ids(SnapshotViewPreview), []string{"a", "b", "d", "readme"}; !slices.Equal(got, want) {
		t.Fatalf("preview = %v, want %v", got, want)
	}

	if _, err := e.Publish(ctx, ContentItemMatch{ID: items["drafts"].ID}); err != nil {
		t.Fatalf("publish folder: %v", err)
	}
	if got, want := ids(SnapshotViewPublished), []string{"a", "d", "readme"}; !slices.Equal(got, want) {
		t.Fatalf("after publishing the folder = %v, want %v", got, want)
	}
	if _, err := e.Unpublish(ctx, ContentItemMatch{ID: items["kb"].ID}); err != nil {
		t.Fatalf("unpublish root folder: %v", err)
	}
	if got, want := ids(SnapshotViewPublished), []string{"readme"}; !slices.Equal(got, want) {
		t.Fatalf("after unpublishing the root folder = %v, want %v", got, want)
	}
}

func TestArchivedSnapshotKeepsPlacement(t *testing.T) {
	e, items := readTree(t)
	snapshot, err := e.GetArchivedSnapshot(context.Background(), ArchivedSnapshotInput{})
	if err != nil || len(snapshot.Items) != 1 {
		t.Fatalf("archived snapshot: %+v, %v", snapshot, err)
	}
	if got := snapshot.Items[0]; got.ParentID != items["kb"].ID || got.Name != "c" {
		t.Fatalf("archived item placement = %q/%q", got.ParentID, got.Name)
	}
}
