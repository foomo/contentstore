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

// MaxFieldLength is the largest value FieldDefinition.MaxLength accepts. A draft and its
// published copy share one MongoDB document (16 MB), so a single value must stay well
// below half of that.
const MaxFieldLength = 2_000_000

// maxDraftFieldsBytes bounds the encoded size of a draft's fields for types that raise
// a field's MaxLength, so the draft and its published copy always fit one document.
const maxDraftFieldsBytes = 7 << 20

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

	// MaxLength overrides the field type's default size limit (bytes for symbol/text,
	// serialized bytes for richtext/json), up to MaxFieldLength. Unlike the Suggested*
	// hints it is enforced. Nil keeps the default.
	MaxLength *int `json:"maxLength,omitempty"`
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
// schema does not already carry one. It panics on an invalid MaxLength, since schemas
// are Go-defined startup configuration.
func (r *Registry) Register(s ContentTypeSchema) {
	for _, f := range s.Fields {
		if err := f.validateMaxLength(); err != nil {
			panic(fmt.Sprintf("contentstore: schema %q field %q: %v", s.Type, f.ID, err))
		}
	}
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
		// Appended only when set so schemas without MaxLength keep their existing hash.
		if f.MaxLength != nil {
			fmt.Fprintf(h, ":max=%d", *f.MaxLength)
		}
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

// hasMaxLength reports whether any field overrides its size limit; only such types are
// subject to the draft size guard.
func (s ContentTypeSchema) hasMaxLength() bool {
	for _, f := range s.Fields {
		if f.MaxLength != nil {
			return true
		}
	}
	return false
}

func (f FieldDefinition) validateMaxLength() error {
	if f.MaxLength == nil {
		return nil
	}
	if fieldLimit(f.Type) == 0 {
		return fmt.Errorf("maxLength is not supported for %s fields", f.Type)
	}
	if *f.MaxLength <= 0 || *f.MaxLength > MaxFieldLength {
		return fmt.Errorf("maxLength must be between 1 and %d", MaxFieldLength)
	}
	return nil
}

// limit returns the field's enforced value-size limit.
func (f FieldDefinition) limit() int {
	if f.MaxLength != nil {
		return *f.MaxLength
	}
	return fieldLimit(f.Type)
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
