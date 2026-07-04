// internal/registry/schemastate.go
package registry

import (
	"log"
	"sync"
)

// SchemaState holds the active SpiceDB schema the registry applied on boot: the
// schema text, its version and source (for GET /v1/schema), and the parsed
// validation model used to vet templates before they are stored.
//
// It is deliberately NOT persisted data. The active schema is boot-time config
// — main() loads it (embedded default or SPICEDB_SCHEMA_PATH override) and
// re-applies it to SpiceDB on every start — so every Store implementation keeps
// it in memory rather than in its durable backend. Both the in-memory store and
// the DynamoDB store embed a SchemaState by value, which promotes SetSchema,
// Schema, and ValidateTemplate onto the store and satisfies that slice of the
// Store contract identically for both, with no duplicated logic.
//
// It carries its own RWMutex so the promoting store never has to coordinate its
// data lock with schema access: SetSchema (boot + rare override) takes the write
// lock; ValidateTemplate (per template create) and Schema (per GET /v1/schema)
// take the read lock.
type SchemaState struct {
	mu      sync.RWMutex
	model   *SchemaModel // parsed validation model; nil disables validation
	text    string
	version string
	source  string
}

// SetSchema records the active schema text/version/source and parses it into a
// validation model. A parse failure disables template validation (model stays
// nil) rather than crashing — the embedded default is known-good, so this only
// degrades gracefully for a malformed override.
func (s *SchemaState) SetSchema(text, version, source string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.text, s.version, s.source = text, version, source
	m, err := ParseSchema(text)
	if err != nil {
		log.Printf("schema parse failed (source=%s); template validation disabled: %v", source, err)
		s.model = nil
		return
	}
	s.model = m
}

// Schema returns the active schema text, version, and source.
func (s *SchemaState) Schema() (text, version, source string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.text, s.version, s.source
}

// ValidateTemplate checks a template's relations against the active schema.
// Returns nil when validation is disabled (no parsed model).
func (s *SchemaState) ValidateTemplate(t AgentTemplate) error {
	s.mu.RLock()
	m := s.model
	s.mu.RUnlock()
	if m == nil {
		return nil
	}
	return m.Validate(t.AuthZ)
}
