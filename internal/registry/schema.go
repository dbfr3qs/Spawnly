// internal/registry/schema.go
package registry

import (
	"fmt"
	"regexp"
	"strings"
)

// SchemaModel is a parsed view of a SpiceDB schema, sufficient for validating a
// template's AuthZSpec.SpiceDBRelations against it before the template is
// stored. It is intentionally minimal: it records each object definition and
// the set of relation + permission names declared on it.
//
// NOTE: this is a pragmatic line-oriented parser, not the full SpiceDB schema
// compiler (github.com/authzed/spicedb/pkg/schemadsl), which is not a
// dependency of this module. It handles the constructs the platform's default
// schema and typical consumer schemas use — `definition`, `relation`,
// `permission`, and `//` line comments — but does not understand caveats or
// every DSL edge case. It is a strict-enough superset check: anything it
// accepts, SpiceDB's WriteSchema is the final authority on.
type SchemaModel struct {
	// definitions maps object-type name -> set of relation/permission names.
	definitions map[string]map[string]struct{}
}

var (
	reLineComment = regexp.MustCompile(`//.*$`)
	reDefinition  = regexp.MustCompile(`^definition\s+(\w+)\s*\{?`)
	reCaveat      = regexp.MustCompile(`^caveat\s+(\w+)`)
	reRelation    = regexp.MustCompile(`^relation\s+(\w+)\s*:`)
	rePermission  = regexp.MustCompile(`^permission\s+(\w+)\s*=`)
)

// ParseSchema parses a SpiceDB schema string into a SchemaModel.
func ParseSchema(schema string) (*SchemaModel, error) {
	m := &SchemaModel{definitions: map[string]map[string]struct{}{}}
	var current string // name of the definition whose body we're inside
	inCaveat := false  // skip caveat bodies; they declare no relations
	depth := 0         // brace depth, to know when a block closes

	for _, raw := range strings.Split(schema, "\n") {
		line := strings.TrimSpace(reLineComment.ReplaceAllString(raw, ""))
		if line == "" {
			continue
		}

		if mt := reDefinition.FindStringSubmatch(line); mt != nil {
			current = mt[1]
			inCaveat = false
			if _, ok := m.definitions[current]; !ok {
				m.definitions[current] = map[string]struct{}{}
			}
		} else if reCaveat.MatchString(line) {
			current = ""
			inCaveat = true
		} else if current != "" && !inCaveat {
			if mt := reRelation.FindStringSubmatch(line); mt != nil {
				m.definitions[current][mt[1]] = struct{}{}
			} else if mt := rePermission.FindStringSubmatch(line); mt != nil {
				m.definitions[current][mt[1]] = struct{}{}
			}
		}

		// Track brace depth so a definition's body ends at its closing brace,
		// even when the opening brace is on the `definition ... {` line.
		depth += strings.Count(line, "{") - strings.Count(line, "}")
		if depth <= 0 {
			current = ""
			inCaveat = false
			depth = 0
		}
	}

	if len(m.definitions) == 0 {
		return nil, fmt.Errorf("schema declares no definitions")
	}
	return m, nil
}

// objectType returns the type prefix of a SpiceDB object reference, stripping
// the ":id" portion (which is a placeholder like {{agent_id}} at template time
// and is never schema-relevant). "tenant:{{tenant_id}}" -> "tenant".
func objectType(ref string) string {
	return strings.SplitN(strings.TrimSpace(ref), ":", 2)[0]
}

// Validate checks every SpiceDBRelationTemplate's Resource/Relation/Subject
// against the schema, ignoring object IDs. It returns a descriptive error
// naming the offending index, field, and unknown type/relation on the first
// failure, or nil if every relation is well-formed against the schema.
func (m *SchemaModel) Validate(spec AuthZSpec) error {
	for i, rel := range spec.SpiceDBRelations {
		resType := objectType(rel.Resource)
		rels, ok := m.definitions[resType]
		if !ok {
			return fmt.Errorf("spiceDbRelations[%d]: unknown resource definition %q", i, resType)
		}
		if _, ok := rels[rel.Relation]; !ok {
			return fmt.Errorf("spiceDbRelations[%d]: unknown relation %q on definition %q", i, rel.Relation, resType)
		}
		subType := objectType(rel.Subject)
		if _, ok := m.definitions[subType]; !ok {
			return fmt.Errorf("spiceDbRelations[%d]: unknown subject definition %q", i, subType)
		}
	}
	return nil
}
