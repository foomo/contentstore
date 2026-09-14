# Content Store

Content Store is a transport-independent Go library for managing schema-validated,
localized content with separate draft and published representations.

It provides:

- draft, publish, unpublish, archive, restore, and delete lifecycles;
- complete published and preview snapshots;
- optimistic concurrency and atomic field updates;
- a schema registry with localized field validation;
- a MongoDB-backed store and an injectable persistence interface;
- checksummed export and atomic replacement operations.

Applications supply their own content schemas, locale configuration, ID generator,
notification adapter, transport, authentication, and logging.

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
