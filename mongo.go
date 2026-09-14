package contentstore

import (
	"fmt"
	"reflect"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/x/mongo/driver/connstring"

	// Register the wire-compression codecs advertised to the server.
	_ "compress/zlib"
	_ "github.com/golang/snappy"
	_ "github.com/klauspost/compress"
)

// mongoCompressors are the wire compressors (and order) negotiated with the
// server, matching the platform default used by other services.
var mongoCompressors = []string{"snappy", "zstd"}

// connect opens a MongoDB client from a standard connection string and returns
// it together with the database name parsed from that string. It is
// dependency-free (only the mongo driver) so the package can be lifted into
// another repository unchanged.
func connect(mongoURL string) (*mongo.Client, string, error) {
	// The driver's ApplyURI does not expose the database name, so parse it here
	// as well (same approach the platform mongostore uses).
	cs, err := connstring.Parse(mongoURL)
	if err != nil {
		return nil, "", fmt.Errorf("contentstore: parse mongo url: %w", err)
	}

	// Map embedded documents to bson.M on decode so generic JSON/richtext field
	// values round-trip as map[string]any (not the default ordered bson.D).
	reg := bson.NewRegistry()
	reg.RegisterTypeMapEntry(bson.TypeEmbeddedDocument, reflect.TypeFor[bson.M]())

	opts := options.Client().
		SetCompressors(mongoCompressors).
		SetRegistry(reg).
		ApplyURI(mongoURL)

	client, err := mongo.Connect(opts)
	if err != nil {
		return nil, "", fmt.Errorf("contentstore: connect mongo: %w", err)
	}
	return client, cs.Database, nil
}
