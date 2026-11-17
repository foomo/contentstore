package contentstore

import (
	"errors"
	"fmt"
	"maps"
	"math"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// TagSet maps tag IDs to weights: how strongly each tag applies to the item. A tag ID is
// "namespace:value", or a bare value in the default namespace. Tags are content: they
// live in the draft snapshot and are published with it. Tags given without a weight weigh
// 1.
type TagSet map[string]float64

const (
	// DefaultMaxTags is the tag limit of a TagPolicy that sets no MaxTags.
	DefaultMaxTags = 100
	// maxTagsLimit is the largest MaxTags a TagPolicy may set.
	maxTagsLimit = 1000
	// maxTagValueLength is the longest tag value, in bytes.
	maxTagValueLength = 200
	// defaultTagWeight is the weight of a tag given without one.
	defaultTagWeight = 1.0
)

var tagNamespacePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// MarshalBSON encodes the set as a document keyed by escaped tag IDs (see escapeTagKey).
// It has a value receiver so it also applies where the set is not addressable.
func (s TagSet) MarshalBSON() ([]byte, error) {
	return bson.Marshal(tagsDocument(s))
}

// UnmarshalBSON decodes a document written by MarshalBSON, replacing the set. Empty input
// (BSON null) decodes to an empty set.
func (s *TagSet) UnmarshalBSON(data []byte) error {
	*s = nil
	if len(data) == 0 {
		return nil
	}
	elements, err := bson.Raw(data).Elements()
	if err != nil {
		return fmt.Errorf("contentstore: decode tags: %w", err)
	}
	out := make(TagSet, len(elements))
	for _, element := range elements {
		id, err := unescapeTagKey(element.Key())
		if err != nil {
			return fmt.Errorf("contentstore: decode tag key %q: %w", element.Key(), err)
		}
		value := element.Value()
		switch value.Type {
		case bson.TypeDouble:
			out[id] = value.Double()
		case bson.TypeInt32:
			out[id] = float64(value.Int32())
		case bson.TypeInt64:
			out[id] = float64(value.Int64())
		default:
			return fmt.Errorf("contentstore: tag %q has a non-numeric weight", id)
		}
	}
	*s = out
	return nil
}

// tagsDocument is the stored form of a tag set: escaped IDs in sorted order.
func tagsDocument(s TagSet) bson.D {
	doc := make(bson.D, 0, len(s))
	for _, id := range slices.Sorted(maps.Keys(s)) {
		doc = append(doc, bson.E{Key: escapeTagKey(id), Value: s[id]})
	}
	return doc
}

// escapeTagKey turns a tag ID into a MongoDB field name that can be addressed by a dotted
// path: '%', '.' and '$' are percent-encoded, and so is the first character of an
// all-digit ID, which MongoDB would otherwise treat as a possible array position (and
// then not use a wildcard index for).
func escapeTagKey(id string) string {
	allDigits := id != "" && strings.IndexFunc(id, func(r rune) bool { return r < '0' || r > '9' }) < 0
	var b strings.Builder
	for i := 0; i < len(id); i++ {
		c := id[i]
		if c == '%' || c == '.' || c == '$' || (i == 0 && allDigits) {
			fmt.Fprintf(&b, "%%%02X", c)
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

func unescapeTagKey(key string) (string, error) {
	return url.PathUnescape(key)
}

// splitTagID returns a tag ID's namespace ("" for a bare value) and value.
func splitTagID(id string) (namespace, value string) {
	if namespace, value, ok := strings.Cut(id, ":"); ok {
		return namespace, value
	}
	return "", id
}

// validateTagID checks a tag ID's syntax: an optional namespace and a valid value.
func validateTagID(id string) error {
	namespace, value := splitTagID(id)
	if strings.Contains(id, ":") && !tagNamespacePattern.MatchString(namespace) {
		return fmt.Errorf("tag %q: invalid namespace %q", id, namespace)
	}
	if err := validateTagValue(value); err != nil {
		return fmt.Errorf("tag %q: %w", id, err)
	}
	return nil
}

func validateTagValue(value string) error {
	switch {
	case value == "":
		return errors.New("value is required")
	case len(value) > maxTagValueLength:
		return fmt.Errorf("value exceeds %d bytes", maxTagValueLength)
	case !utf8.ValidString(value):
		return errors.New("value must be valid UTF-8")
	case strings.ContainsFunc(value, unicode.IsControl):
		return errors.New("value must not contain control characters")
	case strings.TrimSpace(value) != value:
		return errors.New("value must not start or end with spaces")
	}
	return nil
}

// TagPolicy enables tags on a content type: the namespaces its items may use and how many
// tags an item may carry. A type without a policy cannot be tagged.
type TagPolicy struct {
	Namespaces []TagNamespace `json:"namespaces"`
	// MaxTags caps the tags per item; zero means DefaultMaxTags.
	MaxTags int `json:"maxTags,omitempty"`
}

// TagNamespace configures one namespace of a TagPolicy.
type TagNamespace struct {
	// Name is the namespace; empty is the default namespace of bare tag values.
	Name string `json:"name"`
	// Values closes the namespace to these values; empty leaves it open.
	Values []string `json:"values,omitempty"`
	// Weighted lets callers give weights; otherwise every tag of the namespace weighs 1.
	Weighted bool `json:"weighted,omitempty"`
	// MinWeight and MaxWeight bound the weights of a weighted namespace (nil = unbounded).
	// A tag given without a weight weighs 1 and must lie within them too.
	MinWeight *float64 `json:"minWeight,omitempty"`
	MaxWeight *float64 `json:"maxWeight,omitempty"`
}

func (p *TagPolicy) maxTags() int {
	if p.MaxTags == 0 {
		return DefaultMaxTags
	}
	return p.MaxTags
}

func (p *TagPolicy) namespace(name string) (TagNamespace, bool) {
	for _, ns := range p.Namespaces {
		if ns.Name == name {
			return ns, true
		}
	}
	return TagNamespace{}, false
}

// validate checks the policy's own configuration; Register panics on an error.
func (p *TagPolicy) validate() error {
	if p.MaxTags < 0 || p.MaxTags > maxTagsLimit {
		return fmt.Errorf("maxTags must be between 0 and %d", maxTagsLimit)
	}
	seen := map[string]bool{}
	for _, ns := range p.Namespaces {
		if ns.Name != "" && !tagNamespacePattern.MatchString(ns.Name) {
			return fmt.Errorf("invalid tag namespace %q", ns.Name)
		}
		if seen[ns.Name] {
			return fmt.Errorf("duplicate tag namespace %q", ns.Name)
		}
		seen[ns.Name] = true
		values := map[string]bool{}
		for _, value := range ns.Values {
			if err := validateTagValue(value); err != nil {
				return fmt.Errorf("tag namespace %q value %q: %w", ns.Name, value, err)
			}
			if ns.Name == "" && strings.Contains(value, ":") {
				return fmt.Errorf("default tag namespace value %q must not contain ':'", value)
			}
			if values[value] {
				return fmt.Errorf("tag namespace %q: duplicate value %q", ns.Name, value)
			}
			values[value] = true
		}
		if !ns.Weighted && (ns.MinWeight != nil || ns.MaxWeight != nil) {
			return fmt.Errorf("tag namespace %q: weight bounds require a weighted namespace", ns.Name)
		}
		for _, bound := range []*float64{ns.MinWeight, ns.MaxWeight} {
			if bound != nil && !isFinite(*bound) {
				return fmt.Errorf("tag namespace %q: weight bounds must be finite", ns.Name)
			}
		}
		if ns.MinWeight != nil && ns.MaxWeight != nil && *ns.MinWeight > *ns.MaxWeight {
			return fmt.Errorf("tag namespace %q: minWeight exceeds maxWeight", ns.Name)
		}
	}
	return nil
}

// checkTag validates one tag of an item against the policy. weight is nil when the caller
// gave none; it returns the weight to store.
func (p *TagPolicy) checkTag(id string, weight *float64) (float64, error) {
	if err := validateTagID(id); err != nil {
		return 0, err
	}
	name, value := splitTagID(id)
	ns, ok := p.namespace(name)
	if !ok {
		if name == "" {
			return 0, fmt.Errorf("tag %q: bare tags are not allowed", id)
		}
		return 0, fmt.Errorf("tag %q: namespace %q is not allowed", id, name)
	}
	if len(ns.Values) > 0 && !slices.Contains(ns.Values, value) {
		return 0, fmt.Errorf("tag %q: value %q is not allowed in namespace %q", id, value, name)
	}
	w := defaultTagWeight
	if weight != nil {
		if !ns.Weighted {
			return 0, fmt.Errorf("tag %q: namespace %q does not take weights", id, name)
		}
		w = *weight
	}
	if !isFinite(w) {
		return 0, fmt.Errorf("tag %q: weight must be finite", id)
	}
	if (ns.MinWeight != nil && w < *ns.MinWeight) || (ns.MaxWeight != nil && w > *ns.MaxWeight) {
		return 0, fmt.Errorf("tag %q: weight %v is out of range", id, w)
	}
	return w, nil
}

func isFinite(f float64) bool {
	return !math.IsNaN(f) && !math.IsInf(f, 0)
}
