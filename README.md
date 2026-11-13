# Content Store

Content Store is a transport-independent Go library for managing schema-validated,
localized content with separate draft and published representations.

It provides:

- draft, publish, unpublish, archive, restore, and delete lifecycles;
- complete published and preview snapshots;
- optimistic concurrency and atomic field updates;
- a schema registry with localized field validation and optional per-field size limits;
- opt-in content trees (folders) with path lookups and paginated child listings;
- opt-in weighted tags with filters, index-backed rankings and tag counts;
- opt-in scheduled publish, unpublish and archive actions;
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

## Tags

A schema enables tags by declaring a tag policy:

```go
article.Tags = &contentstore.TagPolicy{
	MaxTags: 20, // 0 means 100
	Namespaces: []contentstore.TagNamespace{
		{Name: ""}, // bare tags such as "mongodb"
		{Name: "topic", Weighted: true, MinWeight: new(0.0), MaxWeight: new(1.0)},
		{Name: "audience", Values: []string{"developers", "editors"}}, // closed list
	},
}
```

Types without a policy can't be tagged.

- **Identity.** A tag is a language-neutral ID, `namespace:value` or a bare value. Labels and
  translations belong to the application.
- **Tags are content.** They live in the draft snapshot and publish with it. Each item has one tag
  set, and namespaces stand in for several tag fields.
- **Weight.** A tag's weight is how strongly the tag applies to the item. Unweighted tags weigh 1.
  Item attributes such as ratings or scores belong in fields, not in tags.
- **Writing.** `UpdateTags` (on `ContentTags`) is the only way to write tags: it adds, re-weights,
  removes or replaces them. It's a compare-and-swap on the draft revision, retried on conflict.
  Whole-document writes of tagged items compare-and-swap too, so no concurrent change is lost.
- **Filters.** `GetSnapshot` and `ListChildren` accept a tag filter (all / any / none of a list of
  tags).
- **Rankings.** `ListTagged` ranks the items carrying a tag by its weight, with keyset pagination.
  With `NewMongoStore(ctx, url, contentstore.WithTagIndexes())` the ranking is served from
  compound wildcard indexes, which **need MongoDB 7.0 or later**. Without the option, rankings stay
  correct but are sorted in memory.
- **Counts.** `GetTagCounts` counts tags for facets and autocomplete (with a prefix). It counts in
  memory over the requested types. For very large catalogues it would need an aggregation pipeline
  or a maintained tally instead.

In MongoDB, tag IDs become field names: `%`, `.` and `$` are percent-escaped, and so is the first
character of an all-digit ID. That's transparent to callers.

## Scheduled actions

A scheduled action publishes, unpublishes or archives one item at a given minute. Scheduling is
opt-in on the store:

```go
store, err := contentstore.NewMongoStore(ctx, url, contentstore.WithSchedules())
```

Without the option nothing is created in MongoDB, and the scheduling methods (on
`ContentSchedules`) fail.

- **Scheduling.** `ScheduleAction` takes an item, an action and a time. Times are kept to the
  minute (UTC) and must be in the future.
  - An item takes at most one action per minute: a second one in the same minute is a conflict,
    whatever its action.
  - Scheduling isn't content: it doesn't touch the item's draft revision or state.
- **Running.** The core doesn't run a clock. The application calls `RunDueActions(ctx, time.Now())`
  periodically, e.g. every minute, from every replica.
  - A run takes a single lease first, so one runner works at a time and each item's actions run in
    order. A run that finds the lease taken returns with `Busy` set.
  - Actions go through `Publish`, `Unpublish` and `Archive`, so they validate, guard and emit change
    events exactly like manual calls. A publish takes the draft as it is when the action runs.
  - Actions that were due while nothing ran catch up in order on the next run.
- **Outcomes.** A successful action becomes `succeeded`.
  - An action that fails for a content reason, e.g. a missing required field or an item that no
    longer exists, becomes `failed` with the error and isn't retried.
  - An internal error, e.g. the database being unavailable, returns the action to `pending` with the
    error. The item's later actions wait too, and the next run retries them in order.
  - Actions run at least once: if the application stops mid-action, the next run repeats it.
- **Managing.** `CancelScheduledAction` deletes a pending action. `ListScheduledActions` pages
  through actions by item, status and time range. Executed actions are kept as history.
- **Cascades.** Deleting an item deletes its actions. Archiving it doesn't: a later publish fails
  visibly. `ReplaceContent` clears pending actions, and schedules aren't part of `ExportContent`.

Actions are stored in `content_scheduled_actions` and the lease in `content_schedule_runner`.
Scheduling needs no replica set, though actions on tree items still do, like any tree write.

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
