package contentstore

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
)

// Supported field types.
const (
	FieldTypeSymbol   FieldType = "symbol"
	FieldTypeText     FieldType = "text"
	FieldTypeInteger  FieldType = "integer"
	FieldTypeRichText FieldType = "richtext"
	FieldTypeJSON     FieldType = "json"
)

// FieldEditingFormat describes the value representation expected by an editor.
// It is separate from FieldType so plain text and Markdown can both use text
// storage while remaining distinguishable to clients.
type FieldEditingFormat string

const (
	FieldEditingFormatPlainText          FieldEditingFormat = "plainText"
	FieldEditingFormatContentfulRichText FieldEditingFormat = "contentfulRichText"
	FieldEditingFormatMarkdown           FieldEditingFormat = "markdown"
)

// Per-type value-size limits (serialized chars for richtext/json).
const (
	limitSymbol   = 256
	limitText     = 50_000
	limitRichText = 200_000
	limitJSON     = 200_000
)

// ContentTypeSchema is the authoritative (Go-defined) schema for a content type.
type ContentTypeSchema struct {
	Type    ContentType       `json:"type"`
	Version string            `json:"version"`
	Hash    string            `json:"hash"`
	Fields  []FieldDefinition `json:"fields"`

	// SuppressChangeEvents disables content-change notifications for this type
	// (writes never call the injected Notifier). Default
	// false = notify. It is a behavioural policy, not structural shape, and is
	// deliberately excluded from schemaHash so toggling it never perturbs snapshot
	// hashes. Use it for types with no downstream published-change consumer (e.g. an
	// internal, never-published draft type) so their deletes stay quiet.
	SuppressChangeEvents bool `json:"suppressChangeEvents,omitempty"`
}

// FieldDefinition describes one field. ReadOnly marks system-generated fields editors
// may not author (defaults to editable).
type FieldDefinition struct {
	ID            FieldID            `json:"id"`
	Type          FieldType          `json:"type"`
	EditingFormat FieldEditingFormat `json:"editingFormat,omitempty"`
	Localized     bool               `json:"localized"`
	Required      bool               `json:"required"`
	ReadOnly      bool               `json:"readOnly"`

	Label              string `json:"label,omitempty"`
	Description        string `json:"description,omitempty"`
	ContentHint        string `json:"contentHint,omitempty"`
	FormatHint         string `json:"formatHint,omitempty"`
	SuggestedMinLength *int   `json:"suggestedMinLength,omitempty"`
	SuggestedMaxLength *int   `json:"suggestedMaxLength,omitempty"`
}

// Registry holds the authoritative Go-defined content schemas. It is populated by
// the consuming application at startup (no package-level global), so the core
// carries no project-specific content types.
type Registry struct {
	schemas map[ContentType]ContentTypeSchema
}

// NewRegistry returns an empty schema registry.
func NewRegistry() *Registry {
	return &Registry{schemas: map[ContentType]ContentTypeSchema{}}
}

// Register adds (or replaces) a schema. It computes the structural Hash when the
// schema does not already carry one.
func (r *Registry) Register(s ContentTypeSchema) {
	if s.Hash == "" {
		s.Hash = schemaHash(s)
	}
	r.schemas[s.Type] = s
}

func (r *Registry) schemaFor(t ContentType) (ContentTypeSchema, bool) {
	s, ok := r.schemas[t]
	return s, ok
}

// all returns every registered schema, sorted by type for deterministic output.
func (r *Registry) all() []ContentTypeSchema {
	out := make([]ContentTypeSchema, 0, len(r.schemas))
	for _, s := range r.schemas {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Type < out[j].Type })
	return out
}

// NewSchema builds a schema and computes its structural hash. Consuming
// applications use it to define their content types before registering them.
func NewSchema(t ContentType, version string, fields []FieldDefinition) ContentTypeSchema {
	s := ContentTypeSchema{Type: t, Version: version, Fields: fields}
	s.Hash = schemaHash(s)
	return s
}

// schemaHash is a deterministic fingerprint of a schema's structural shape, stored on
// snapshots for diagnostics and future migration work.
func schemaHash(s ContentTypeSchema) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s|%s", s.Type, s.Version)
	for _, f := range s.Fields {
		fmt.Fprintf(h, "|%s:%s:%t:%t:%t", f.ID, f.Type, f.Localized, f.Required, f.ReadOnly)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func (s ContentTypeSchema) field(id FieldID) (FieldDefinition, bool) {
	for _, f := range s.Fields {
		if f.ID == id {
			return f, true
		}
	}
	return FieldDefinition{}, false
}

// fieldLimit returns the value-size limit for a field type.
func fieldLimit(t FieldType) int {
	switch t {
	case FieldTypeSymbol:
		return limitSymbol
	case FieldTypeText:
		return limitText
	case FieldTypeRichText:
		return limitRichText
	case FieldTypeJSON:
		return limitJSON
	default:
		return 0
	}
}

// OptionalInt returns a pointer to v, for optional schema length hints.
//
//go:fix inline
func OptionalInt(v int) *int { return new(v) }
