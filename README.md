# Content Store

Content Store is a transport-independent Go library for managing schema-validated,
localized content with separate draft and published representations.

It provides:

- draft, publish, unpublish, archive, restore, and delete lifecycles;
- complete published and preview snapshots;
- optimistic concurrency and atomic field updates;
- a schema registry with localized field validation and optional per-field size limits;
- opt-in content trees (folders) with path lookups and paginated child listings;
- a MongoDB-backed store and an injectable persistence interface;
- checksummed export and atomic replacement operations.

Applications supply their own content schemas, locale configuration, ID generator,
notification adapter, transport, authentication, and logging.

## Content trees

A schema becomes a tree container by listing the types it may contain:

```go
folder := contentstore.NewSchema("folder", "1", fields)
folder.ChildTypes = []contentstore.ContentType{"folder", "document"}
registry.Register(folder)
```

Containers and their child types are *tree types*. Their items carry a `ParentID`
(empty at root level) and a `Name` that is unique within the parent. They form a
single tree:

- `SaveDraft` places an item when it creates it; `Move` (on `ContentTree`) moves or
  renames it.
- Placement is structural and not versioned: a move applies to the draft and published
  views at once and leaves the draft revision untouched.
- Folders follow draft/publish like any other item. In the published view an item is
  visible only while it and all its ancestors are published; archived items and
  everything under them are never visible.
- A tree item can't be deleted while it has children, or archived while it has children
  that aren't archived. Nothing cascades.
- `ListChildren` and `GetTreeItem` (by ID, key or path such as `/kb/tables/users`) read the
  tree per request with field projection and name-ordered pagination.

Tree writes run in MongoDB transactions serialized through a lock document, so
**tree types need a replica set** (a single-node replica set is enough). Content types
that aren't tree types are unaffected.

Placement changes don't touch snapshot timestamps: moving an item, or unpublishing one of
its ancestors, doesn't change the `UpdatedAt` of the items it affects. Use change events
(`OpMove`, `OpUnpublish`, ...) to detect tree changes.

## Field size limits

Each field type has a default size limit: 256 bytes for `symbol`, 50,000 bytes for `text`,
200,000 serialized bytes for `richtext` and `json`. `FieldDefinition.MaxLength` overrides
it, up to `MaxFieldLength` (2,000,000). A draft and its published copy share one MongoDB
document (16 MB), so drafts of types that set `MaxLength` are also limited to 7 MiB of
encoded fields.

## Installation

```bash
go get github.com/foomo/contentstore
```

## Testing

```bash
go test ./...
```

The MongoDB integration tests use `mongodb://localhost:27017` when available and
skip automatically otherwise.
