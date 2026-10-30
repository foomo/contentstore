package contentstore

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/readconcern"
	"go.mongodb.org/mongo-driver/v2/mongo/readpref"
	"go.mongodb.org/mongo-driver/v2/mongo/writeconcern"
)

const collectionContentItems = "content_items"

// collectionTreeLock holds the single document every tree transaction increments first,
// which serializes tree writes. It is created on first tree use only.
const (
	collectionTreeLock = "content_tree_lock"
	treeLockID         = "tree"
)

// Persistor is the MongoDB-backed Store. It depends only on the mongo driver.
type Persistor struct {
	client *mongo.Client
	coll   *mongo.Collection

	treeLockMu    sync.Mutex
	treeLockReady bool
}

var _ TreeStore = (*Persistor)(nil)

// NewMongoStore connects to MongoDB (database taken from the connection string) and
// ensures the content_items indexes, returning a ready Store.
func NewMongoStore(ctx context.Context, mongoURL string) (*Persistor, error) {
	client, db, err := connect(mongoURL)
	if err != nil {
		return nil, err
	}
	p := &Persistor{client: client, coll: client.Database(db).Collection(collectionContentItems)}
	if err := p.ensureIndexes(ctx); err != nil {
		_ = client.Disconnect(context.Background())
		return nil, err
	}
	return p, nil
}

// Close disconnects the underlying client.
func (p *Persistor) Close() error { return p.client.Disconnect(context.Background()) }

func (p *Persistor) ensureIndexes(ctx context.Context) error {
	return ensureIndexes(ctx, p.coll)
}

func ensureIndexes(ctx context.Context, coll *mongo.Collection) error {
	_, err := coll.Indexes().CreateMany(ctx, []mongo.IndexModel{
		// Unique key where present (partial: excludes items with no/empty key).
		{
			Keys: bson.D{{Key: "key", Value: 1}},
			Options: options.Index().SetName("key_unique").SetUnique(true).
				SetPartialFilterExpression(bson.D{{Key: "key", Value: bson.D{{Key: "$gt", Value: ""}}}}),
		},
		// Owner/type reconciliation and implicit lookup.
		{
			Keys: bson.D{
				{Key: "ownerRef.source", Value: 1},
				{Key: "ownerRef.type", Value: 1},
				{Key: "ownerRef.id", Value: 1},
				{Key: "type", Value: 1},
			},
			Options: options.Index().SetName("ownerRef_source_type_id_type"),
		},
		// Type-filtered snapshots and tooling.
		{
			Keys:    bson.D{{Key: "type", Value: 1}},
			Options: options.Index().SetName("type"),
		},
		// Tree names unique within a parent (a missing parentId is root level), and
		// name-ordered child listing. Partial: excludes every item without a name, which
		// is every item of a non-tree type.
		{
			Keys: bson.D{{Key: "parentId", Value: 1}, {Key: "name", Value: 1}},
			Options: options.Index().SetName("parent_name_unique").SetUnique(true).
				SetPartialFilterExpression(bson.D{{Key: "name", Value: bson.D{{Key: "$gt", Value: ""}}}}),
		},
	})
	if err != nil {
		return fmt.Errorf("contentstore: ensure indexes: %w", err)
	}
	return nil
}

// ExportAll reads every content document from one MongoDB snapshot transaction.
func (p *Persistor) ExportAll(ctx context.Context) ([]*ContentItem, error) {
	session, err := p.client.StartSession()
	if err != nil {
		return nil, fmt.Errorf("contentstore: start export session: %w", err)
	}
	defer session.EndSession(ctx)

	var items []*ContentItem
	_, err = session.WithTransaction(ctx, func(txCtx context.Context) (any, error) {
		found, findErr := findCollection(txCtx, p.coll, bson.D{})
		if findErr != nil {
			return nil, findErr
		}
		items = found
		return nil, nil
	}, options.Transaction().
		SetReadConcern(readconcern.Snapshot()).
		SetReadPreference(readpref.Primary()))
	if err != nil {
		return nil, fmt.Errorf("contentstore: export snapshot transaction: %w", err)
	}
	return items, nil
}

// ReplaceAll builds and verifies a temporary collection, then atomically renames
// it over content_items. Any failure before the rename leaves the live collection
// untouched.
func (p *Persistor) ReplaceAll(ctx context.Context, items []*ContentItem) error {
	if err := validateContentItems(items); err != nil {
		return err
	}
	tempName, err := replacementCollectionName()
	if err != nil {
		return fmt.Errorf("contentstore: create replacement collection name: %w", err)
	}
	database := p.coll.Database()
	if err := database.CreateCollection(ctx, tempName); err != nil {
		return fmt.Errorf("contentstore: create replacement collection: %w", err)
	}
	temp := database.Collection(tempName)
	swapped := false
	defer func() {
		if swapped {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = temp.Drop(cleanupCtx)
	}()

	if err := ensureIndexes(ctx, temp); err != nil {
		return fmt.Errorf("contentstore: prepare replacement collection: %w", err)
	}
	if len(items) > 0 {
		documents := make([]any, len(items))
		for i := range items {
			documents[i] = items[i]
		}
		if _, err := temp.InsertMany(ctx, documents); err != nil {
			return fmt.Errorf("contentstore: insert replacement items: %w", err)
		}
	}

	stored, err := findCollection(ctx, temp, bson.D{})
	if err != nil {
		return fmt.Errorf("contentstore: verify replacement collection: %w", err)
	}
	if len(stored) != len(items) {
		return fmt.Errorf("contentstore: replacement count mismatch: wrote %d items, read %d", len(items), len(stored))
	}
	wantChecksum, err := contentItemsChecksum(items)
	if err != nil {
		return fmt.Errorf("contentstore: checksum replacement input: %w", err)
	}
	gotChecksum, err := contentItemsChecksum(stored)
	if err != nil {
		return fmt.Errorf("contentstore: checksum replacement collection: %w", err)
	}
	if gotChecksum != wantChecksum {
		return fmt.Errorf("contentstore: replacement checksum mismatch after persistence")
	}

	source := database.Name() + "." + tempName
	target := database.Name() + "." + collectionContentItems
	if err := p.client.Database("admin").RunCommand(ctx, bson.D{
		{Key: "renameCollection", Value: source},
		{Key: "to", Value: target},
		{Key: "dropTarget", Value: true},
	}).Err(); err != nil {
		return fmt.Errorf("contentstore: atomically install replacement collection: %w", err)
	}
	swapped = true
	return nil
}

func replacementCollectionName() (string, error) {
	var suffix [12]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return "", err
	}
	return collectionContentItems + "_replacement_" + hex.EncodeToString(suffix[:]), nil
}

// GetByID returns the item with the given ID, or ErrNotFound.
func (p *Persistor) GetByID(ctx context.Context, id string) (*ContentItem, error) {
	item := &ContentItem{}
	if err := p.coll.FindOne(ctx, bson.D{{Key: "_id", Value: id}}).Decode(item); err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("contentstore: get by id: %w", err)
	}
	return item, nil
}

// GetByKey returns the item with the given key, or ErrNotFound.
func (p *Persistor) GetByKey(ctx context.Context, key string) (*ContentItem, error) {
	item := &ContentItem{}
	if err := p.coll.FindOne(ctx, bson.D{{Key: "key", Value: key}}).Decode(item); err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("contentstore: get by key: %w", err)
	}
	return item, nil
}

// Upsert inserts or replaces the item's document (keyed by _id). A nil expected
// revision preserves unconditional upsert behavior; zero is create-only; a positive
// value replaces only when the stored draft revision matches. It uses a full document
// replacement so fields cleared through omitempty are actually removed.
func (p *Persistor) Upsert(ctx context.Context, item *ContentItem, expectedDraftRevision *int64) error {
	if expectedDraftRevision != nil && *expectedDraftRevision == 0 {
		_, err := p.coll.InsertOne(ctx, item)
		if mongo.IsDuplicateKeyError(err) {
			return ErrConflict
		}
		if err != nil {
			return fmt.Errorf("contentstore: insert: %w", err)
		}
		return nil
	}

	filter := draftReplaceFilter(item.ID, expectedDraftRevision)
	opts := options.Replace().SetUpsert(expectedDraftRevision == nil)
	result, err := p.coll.ReplaceOne(ctx, filter, item, opts)
	if err != nil {
		return fmt.Errorf("contentstore: upsert: %w", err)
	}
	if expectedDraftRevision != nil && result.MatchedCount == 0 {
		return ErrConflict
	}
	return nil
}

func draftReplaceFilter(itemID string, expectedDraftRevision *int64) bson.D {
	filter := bson.D{{Key: "_id", Value: itemID}}
	if expectedDraftRevision != nil {
		filter = append(filter, bson.E{Key: "draft.revision", Value: *expectedDraftRevision})
	}
	return filter
}

// SetField atomically merges one field/locale into the item's draft, creating the item
// (keyed by Match.Key) when absent. Unlike Upsert it does not read-modify-write the whole
// document: the merge is a server-side $set on draft.fields.<fieldID>.<locale>, so a burst
// of concurrent per-field writes all land instead of clobbering each other. A nil value
// clears that locale.
func (p *Persistor) SetField(ctx context.Context, in FieldUpdate) (*ContentItem, error) {
	filter, upsert := fieldUpdateFilter(in.Match, in.Type)
	update := fieldUpdateDocument(in)
	opts := options.FindOneAndUpdate().SetReturnDocument(options.After).SetUpsert(upsert)

	item := &ContentItem{}
	err := p.coll.FindOneAndUpdate(ctx, filter, update, opts).Decode(item)
	if err != nil && upsert && mongo.IsDuplicateKeyError(err) {
		// Lost a concurrent create race on the same key: the document now exists, so a
		// single retry resolves to it and applies the field update instead of inserting.
		err = p.coll.FindOneAndUpdate(ctx, filter, update, opts).Decode(item)
		if mongo.IsDuplicateKeyError(err) {
			// The key is held by an item of another type.
			return nil, ErrConflict
		}
	}
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("contentstore: set field: %w", err)
	}
	return item, nil
}

// fieldUpdateFilter selects the target document and whether a missing one is created. A
// key match upserts (create-or-update); an id-only match must resolve to an existing item.
// The key match also requires the type, so an item of another type created concurrently
// under the same key makes the upsert fail on the unique key instead of having its type
// overwritten.
func fieldUpdateFilter(m ContentItemMatch, t ContentType) (bson.D, bool) {
	if m.Key != "" {
		return bson.D{{Key: "key", Value: m.Key}, {Key: "type", Value: t}}, true
	}
	return bson.D{{Key: "_id", Value: m.ID}}, false
}

// fieldUpdateDocument builds the targeted update: set exactly the one field/locale path
// (or unset it for a nil value), bump the draft revision, and set creation-only fields on
// insert. It deliberately never sets draft.fields wholesale, so concurrent writes to other
// fields are preserved.
func fieldUpdateDocument(in FieldUpdate) bson.D {
	fieldPath := "draft.fields." + in.FieldID + "." + in.Locale
	set := bson.D{
		{Key: "type", Value: in.Type},
		{Key: "updatedAt", Value: in.Now},
		{Key: "draft.schemaVersion", Value: in.SchemaVersion},
		{Key: "draft.schemaHash", Value: in.SchemaHash},
		{Key: "draft.updatedAt", Value: in.Now},
	}
	update := bson.D{}
	if in.Value == nil {
		update = append(update, bson.E{Key: "$unset", Value: bson.D{{Key: fieldPath, Value: ""}}})
	} else {
		set = append(set, bson.E{Key: fieldPath, Value: in.Value})
	}
	setOnInsert := bson.D{
		{Key: "_id", Value: in.NewID},
		{Key: "createdAt", Value: in.Now},
		{Key: "draft.createdAt", Value: in.Now},
	}
	if in.OwnerRef != nil {
		setOnInsert = append(setOnInsert, bson.E{Key: "ownerRef", Value: in.OwnerRef})
	}
	update = append(update,
		bson.E{Key: "$set", Value: set},
		bson.E{Key: "$setOnInsert", Value: setOnInsert},
		bson.E{Key: "$inc", Value: bson.D{{Key: "draft.revision", Value: int64(1)}}},
	)
	return update
}

// Delete hard-deletes the item by ID. Deleting a missing item is not an error.
func (p *Persistor) Delete(ctx context.Context, id string) error {
	_, err := p.coll.DeleteOne(ctx, bson.D{{Key: "_id", Value: id}})
	if err != nil {
		return fmt.Errorf("contentstore: delete: %w", err)
	}
	return nil
}

// FindPublished returns all published, non-archived items, optionally narrowed by type
// and exact owner.
func (p *Persistor) FindPublished(ctx context.Context, types []ContentType, ownerRef *OwnerRef) ([]*ContentItem, error) {
	filter := bson.D{
		{Key: "published", Value: bson.D{{Key: "$ne", Value: nil}}},
		{Key: "archivedAt", Value: nil},
	}
	if len(types) > 0 {
		filter = append(filter, bson.E{Key: "type", Value: bson.D{{Key: "$in", Value: types}}})
	}
	if ownerRef != nil {
		filter = append(filter,
			bson.E{Key: "ownerRef.source", Value: ownerRef.Source},
			bson.E{Key: "ownerRef.type", Value: ownerRef.Type},
			bson.E{Key: "ownerRef.id", Value: ownerRef.ID},
		)
	}
	return p.find(ctx, filter)
}

// FindAll returns non-archived items (draft, changed, and published), optionally
// narrowed by type and exact owner. It backs the editing-oriented draft snapshot;
// archived items are excluded.
func (p *Persistor) FindAll(ctx context.Context, types []ContentType, ownerRef *OwnerRef) ([]*ContentItem, error) {
	filter := bson.D{
		{Key: "archivedAt", Value: nil},
	}
	if len(types) > 0 {
		filter = append(filter, bson.E{Key: "type", Value: bson.D{{Key: "$in", Value: types}}})
	}
	if ownerRef != nil {
		filter = append(filter,
			bson.E{Key: "ownerRef.source", Value: ownerRef.Source},
			bson.E{Key: "ownerRef.type", Value: ownerRef.Type},
			bson.E{Key: "ownerRef.id", Value: ownerRef.ID},
		)
	}
	return p.find(ctx, filter)
}

// FindArchived returns all archived items, optionally narrowed to the given types (all when
// empty). It is the counterpart to FindPublished/FindAll, which both exclude archived items.
func (p *Persistor) FindArchived(ctx context.Context, types []ContentType) ([]*ContentItem, error) {
	filter := bson.D{
		{Key: "archivedAt", Value: bson.D{{Key: "$ne", Value: nil}}},
	}
	if len(types) > 0 {
		filter = append(filter, bson.E{Key: "type", Value: bson.D{{Key: "$in", Value: types}}})
	}
	return p.find(ctx, filter)
}

func (p *Persistor) find(ctx context.Context, filter bson.D) ([]*ContentItem, error) {
	return findCollection(ctx, p.coll, filter)
}

func findCollection(ctx context.Context, coll *mongo.Collection, filter bson.D) ([]*ContentItem, error) {
	cur, err := coll.Find(ctx, filter)
	if err != nil {
		return nil, fmt.Errorf("contentstore: find: %w", err)
	}
	defer cur.Close(context.Background())

	var items []*ContentItem
	if err := cur.All(ctx, &items); err != nil {
		return nil, fmt.Errorf("contentstore: find decode: %w", err)
	}
	return items, nil
}

// RunTreeTransaction runs fn in a MongoDB transaction whose first write increments the
// tree lock document. Concurrent tree transactions therefore write-conflict on that
// document and the driver retries the loser after the winner commits, which serializes
// tree writes: checks such as "parent exists" or "no cycle" cannot be invalidated by a
// concurrent tree write before fn commits. Requires a replica set.
func (p *Persistor) RunTreeTransaction(ctx context.Context, fn func(ctx context.Context) error) error {
	if err := p.ensureTreeLock(ctx); err != nil {
		return err
	}
	session, err := p.client.StartSession()
	if err != nil {
		return fmt.Errorf("contentstore: start tree session: %w", err)
	}
	defer session.EndSession(ctx)

	lock := p.coll.Database().Collection(collectionTreeLock)
	_, err = session.WithTransaction(ctx, func(txCtx context.Context) (any, error) {
		res, err := lock.UpdateOne(txCtx, bson.D{{Key: "_id", Value: treeLockID}}, bson.D{{Key: "$inc", Value: bson.D{{Key: "seq", Value: int64(1)}}}})
		if err != nil {
			return nil, fmt.Errorf("contentstore: acquire tree lock: %w", err)
		}
		if res.MatchedCount == 0 {
			// Without the lock document nothing would serialize this transaction.
			p.treeLockMu.Lock()
			p.treeLockReady = false
			p.treeLockMu.Unlock()
			return nil, errors.New("contentstore: tree lock document is missing")
		}
		return nil, fn(txCtx)
	}, options.Transaction().
		SetReadConcern(readconcern.Snapshot()).
		SetWriteConcern(writeconcern.Majority()).
		SetReadPreference(readpref.Primary()))
	if err != nil {
		return fmt.Errorf("contentstore: tree transaction: %w", err)
	}
	return nil
}

// ensureTreeLock creates the lock collection and document outside any transaction, once
// per Persistor; a failed attempt is retried on the next call. Concurrent creation by
// other instances is tolerated.
func (p *Persistor) ensureTreeLock(ctx context.Context) error {
	p.treeLockMu.Lock()
	defer p.treeLockMu.Unlock()
	if p.treeLockReady {
		return nil
	}
	database := p.coll.Database()
	if err := database.CreateCollection(ctx, collectionTreeLock); err != nil && !isNamespaceExists(err) {
		return fmt.Errorf("contentstore: create tree lock collection: %w", err)
	}
	_, err := database.Collection(collectionTreeLock).UpdateOne(ctx,
		bson.D{{Key: "_id", Value: treeLockID}},
		bson.D{{Key: "$setOnInsert", Value: bson.D{{Key: "seq", Value: int64(0)}}}},
		options.UpdateOne().SetUpsert(true))
	if err != nil && !mongo.IsDuplicateKeyError(err) {
		return fmt.Errorf("contentstore: create tree lock: %w", err)
	}
	p.treeLockReady = true
	return nil
}

// isNamespaceExists reports MongoDB's NamespaceExists error (code 48).
func isNamespaceExists(err error) bool {
	serverErr, ok := errors.AsType[mongo.ServerError](err)
	return ok && serverErr.HasErrorCode(48)
}

// FindChildren returns the items placed under q.ParentID (named root-level items when
// empty), ordered by name.
func (p *Persistor) FindChildren(ctx context.Context, q ChildQuery) ([]*ContentItem, error) {
	opts := options.Find().SetSort(bson.D{{Key: "name", Value: 1}})
	if q.Limit > 0 {
		opts.SetLimit(int64(q.Limit))
	}
	if q.Fields != nil {
		opts.SetProjection(childProjection(q.Fields))
	}
	cur, err := p.coll.Find(ctx, childFilter(q), opts)
	if err != nil {
		return nil, fmt.Errorf("contentstore: find children: %w", err)
	}
	defer cur.Close(context.Background())

	var items []*ContentItem
	if err := cur.All(ctx, &items); err != nil {
		return nil, fmt.Errorf("contentstore: find children decode: %w", err)
	}
	return items, nil
}

// childFilter builds the FindChildren filter. It always constrains name to be non-empty
// so the query can use the partial parent_name_unique index.
func childFilter(q ChildQuery) bson.D {
	var parent any = q.ParentID
	if q.ParentID == "" {
		parent = nil // matches a missing parentId: root level
	}
	name := bson.D{{Key: "$gt", Value: q.AfterName}}
	if q.Name != "" {
		name = append(name, bson.E{Key: "$eq", Value: q.Name})
	}
	filter := bson.D{
		{Key: "parentId", Value: parent},
		{Key: "name", Value: name},
	}
	switch q.Lifecycle {
	case ChildrenLive:
		filter = append(filter, bson.E{Key: "archivedAt", Value: nil})
	case ChildrenPublished:
		filter = append(filter,
			bson.E{Key: "published", Value: bson.D{{Key: "$ne", Value: nil}}},
			bson.E{Key: "archivedAt", Value: nil},
		)
	}
	if len(q.Types) > 0 {
		filter = append(filter, bson.E{Key: "type", Value: bson.D{{Key: "$in", Value: q.Types}}})
	}
	return filter
}

// childProjection keeps everything but the snapshot field sets, of which it keeps only
// the requested fields (none for an empty list).
func childProjection(fields []FieldID) bson.D {
	projection := bson.D{
		{Key: "key", Value: 1}, {Key: "type", Value: 1}, {Key: "ownerRef", Value: 1},
		{Key: "parentId", Value: 1}, {Key: "name", Value: 1}, {Key: "archivedAt", Value: 1},
		{Key: "createdAt", Value: 1}, {Key: "updatedAt", Value: 1},
	}
	for _, snapshot := range []string{"draft", "published"} {
		for _, f := range []string{"revision", "schemaVersion", "schemaHash", "createdAt", "updatedAt"} {
			projection = append(projection, bson.E{Key: snapshot + "." + f, Value: 1})
		}
		for _, f := range fields {
			projection = append(projection, bson.E{Key: snapshot + ".fields." + string(f), Value: 1})
		}
	}
	return projection
}
