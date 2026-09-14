package contentstore

import (
	"encoding/json"
	"fmt"
	"math"
)

// validateDraft checks a draft's structure, field types/sizes, and locale usage. It does
// NOT enforce required fields — that happens on publish.
func (e *Engine) validateDraft(t ContentType, fields ContentFields) error {
	schema, ok := e.schemas.schemaFor(t)
	if !ok {
		return badRequest(fmt.Sprintf("unknown content type %q", t))
	}
	def := e.locales.Default
	for fieldID, byLocale := range fields {
		fd, ok := schema.field(FieldID(fieldID))
		if !ok {
			return badRequest(fmt.Sprintf("unknown field %q for type %q", fieldID, t))
		}
		for locale, value := range byLocale {
			if !e.locales.isSupported(locale) {
				return badRequest(fmt.Sprintf("field %q: unsupported locale %q", fieldID, locale))
			}
			if !fd.Localized && locale != def {
				return badRequest(fmt.Sprintf("non-localized field %q may only use the default locale %q", fieldID, def))
			}
			if err := validateValue(fd, value); err != nil {
				return badRequest(fmt.Sprintf("field %q locale %q: %s", fieldID, locale, err))
			}
		}
	}
	return nil
}

// validateField checks a single field/locale value (used by the partial SetFieldValue
// write). A nil value is allowed (it clears the locale).
func (e *Engine) validateField(t ContentType, fieldID, locale string, value FieldValue) error {
	schema, ok := e.schemas.schemaFor(t)
	if !ok {
		return badRequest(fmt.Sprintf("unknown content type %q", t))
	}
	fd, ok := schema.field(FieldID(fieldID))
	if !ok {
		return badRequest(fmt.Sprintf("unknown field %q for type %q", fieldID, t))
	}
	if !e.locales.isSupported(locale) {
		return badRequest(fmt.Sprintf("field %q: unsupported locale %q", fieldID, locale))
	}
	if !fd.Localized && locale != e.locales.Default {
		return badRequest(fmt.Sprintf("non-localized field %q may only use the default locale %q", fieldID, e.locales.Default))
	}
	if err := validateValue(fd, value); err != nil {
		return badRequest(fmt.Sprintf("field %q locale %q: %s", fieldID, locale, err))
	}
	return nil
}

// validatePublish enforces required fields on the snapshot about to be published: a
// required field must have a value in the default locale.
func (e *Engine) validatePublish(t ContentType, fields ContentFields) error {
	schema, ok := e.schemas.schemaFor(t)
	if !ok {
		return badRequest(fmt.Sprintf("unknown content type %q", t))
	}
	def := e.locales.Default
	for _, fd := range schema.Fields {
		if !fd.Required {
			continue
		}
		if !hasValue(fields[string(fd.ID)][def]) {
			return notAcceptable(fmt.Sprintf("required field %q must have a default-locale value to publish", fd.ID))
		}
	}
	return nil
}

// validateValue enforces the field type and size limit for a single locale value. A nil
// value clears the locale and is always allowed.
func validateValue(fd FieldDefinition, value FieldValue) error {
	if value == nil {
		return nil
	}
	switch fd.Type {
	case FieldTypeSymbol, FieldTypeText:
		s, ok := value.(string)
		if !ok {
			return fmt.Errorf("expected a string")
		}
		if limit := fieldLimit(fd.Type); len(s) > limit {
			return fmt.Errorf("exceeds %d characters", limit)
		}
	case FieldTypeInteger:
		if !isInteger(value) {
			return fmt.Errorf("expected an integer")
		}
	case FieldTypeRichText, FieldTypeJSON:
		if _, ok := value.(map[string]any); !ok {
			return fmt.Errorf("expected a JSON object")
		}
		b, err := json.Marshal(value)
		if err != nil {
			return fmt.Errorf("not serializable: %w", err)
		}
		if limit := fieldLimit(fd.Type); len(b) > limit {
			return fmt.Errorf("serialized value exceeds %d characters", limit)
		}
	default:
		return fmt.Errorf("unknown field type %q", fd.Type)
	}
	return nil
}

func isInteger(value any) bool {
	switch n := value.(type) {
	case int, int8, int16, int32, int64:
		return true
	case uint:
		return uint64(n) <= math.MaxInt64
	case uint8, uint16, uint32:
		return true
	case uint64:
		return n <= math.MaxInt64
	case float32:
		value64 := float64(n)
		return !math.IsNaN(value64) && !math.IsInf(value64, 0) && math.Trunc(value64) == value64 && value64 >= math.MinInt64 && value64 < -float64(math.MinInt64)
	case float64:
		return !math.IsNaN(n) && !math.IsInf(n, 0) && math.Trunc(n) == n && n >= math.MinInt64 && n < -float64(math.MinInt64)
	case json.Number:
		_, err := n.Int64()
		return err == nil
	default:
		return false
	}
}

func hasValue(v FieldValue) bool {
	if v == nil {
		return false
	}
	if s, ok := v.(string); ok {
		return s != ""
	}
	return true
}
