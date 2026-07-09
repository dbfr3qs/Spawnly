// Package dynamostore is a durable, single-table DynamoDB implementation of the
// registry's neutral registry.Store contract, used on the AWS deploy where the
// in-memory maps would lose every agent, template, consent, and event on a pod
// restart or cluster rebuild.
//
// Dependency direction: this adapter depends on the registry contract and the
// AWS SDK, never the reverse — no handler or platform code knows DynamoDB
// exists. Backend choice is wiring in cmd/registry (STORE=dynamodb) + deploy
// config only, so the kind/local path stays byte-for-byte on the in-memory store.
//
// # Single-table design
//
// One table with generic keys pk/sk and two ALL-projected GSIs (gsi1pk/gsi1sk,
// gsi2pk/gsi2sk — only gsi2pk is used) serves every access pattern:
//
//	Entity          pk                         sk                        gsi1pk/gsi1sk              gsi2pk
//	Template        TPL#<agentType>            META                      TPL / <agentType>         —
//	Agent           AG#<agentId>               META                      AG / <agentId>            PARENT#<parentId> (sparse)
//	Event           AG#<agentId>               EV#<%020d nano>#<rand>    —                         —
//	Consent         CON#<user>|<parent>|<child> META                     CON / <user>|<parent>|<child>  CONID#<consentId>
//	Consent request CREQ#<id>                  META                      CREQ / <userId>#<created>#<id>  EDGE#<user>|<parent>|<child>
//	Open marker     OPEN#<openRequestKey>      META                      —                         —
//	Organization    ORG#<orgId>                META                      ORG / <orgId>             —
//
// Access patterns: point gets = GetItem; list-by-type = Query gsi1 on the type
// partition; ListChildren = Query gsi2 PARENT#<id>; GetEvents = Query pk +
// begins_with(sk,"EV#") (the zero-padded-nano SK keeps append order, the random
// suffix disambiguates same-nanosecond appends); consent-by-id = Query gsi2
// CONID#<id>; the approve-sweep = Query gsi2 EDGE#<edge>. Disabled-template and
// dismissed-agent exclusion + the consent-request status filter are applied in
// Go after the Query: at beta scale each type partition is tiny, and Go-side
// filtering sidesteps fragile FilterExpressions over optional/omitempty
// attributes (the GSI-partition-per-type is the noted scaling caveat, not the
// filter mechanism).
//
// The active SpiceDB schema is NOT stored here — it is boot-time config the
// embedded registry.SchemaState keeps in memory, re-applied on every start (see
// SchemaState), so SetSchema/Schema/ValidateTemplate are shared verbatim with
// the in-memory store.
package dynamostore

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/spawnly/platform/internal/events"
	"github.com/spawnly/platform/internal/registry"
)

// Store is the DynamoDB-backed registry.Store. It embeds registry.SchemaState so
// SetSchema/Schema/ValidateTemplate come from the shared config type.
type Store struct {
	registry.SchemaState
	db    *dynamodb.Client
	table string

	// afterPivot is a test-only seam invoked inside ResolveConsentRequest between
	// the status pivot and the open-marker cleanup, so a test can interleave a
	// concurrent CreateConsentRequest in exactly that window (the marker-clobber
	// race of finding 2). nil in production — never set outside tests.
	afterPivot func()
}

// New builds a Store over the given AWS config and table name. optFns lets the
// caller override the resolved endpoint (DYNAMODB_ENDPOINT → dynamodb-local for
// tests/local runs); in AWS the SDK resolves the real endpoint and Pod Identity
// supplies credentials — no static keys.
func New(cfg aws.Config, table string, optFns ...func(*dynamodb.Options)) *Store {
	return &Store{db: dynamodb.NewFromConfig(cfg, optFns...), table: table}
}

// Compile-time proof the DynamoDB store satisfies the neutral contract.
var _ registry.Store = (*Store)(nil)

// ---- keys, constants, small helpers -------------------------------------------------

const (
	skMeta = "META"

	gsi1Template = "TPL"
	gsi1Agent    = "AG"
	gsi1Consent  = "CON"
	gsi1Creq     = "CREQ"
	gsi1Org      = "ORG"
)

func tplPK(agentType string) string { return "TPL#" + agentType }
func agPK(agentID string) string    { return "AG#" + agentID }
func conPK(edge string) string      { return "CON#" + edge }
func creqPK(id string) string       { return "CREQ#" + id }
func openPK(key string) string      { return "OPEN#" + key }
func orgPK(id string) string        { return "ORG#" + id }

// edgeKey / openRequestKey delegate to the shared registry key builders so this
// store and the in-memory store serialise consent edges identically (single
// source of truth in the registry contract).
func edgeKey(userID, parentType, childType string) string {
	return registry.ConsentEdgeKey(userID, parentType, childType)
}

func openRequestKey(userID, parentType, childType, agentID string) string {
	return registry.OpenRequestKey(userID, parentType, childType, agentID)
}

func strAV(s string) types.AttributeValue     { return &types.AttributeValueMemberS{Value: s} }
func timeAV(t time.Time) types.AttributeValue { return strAV(t.Format(time.RFC3339Nano)) }

func keyOf(pk, sk string) map[string]types.AttributeValue {
	return map[string]types.AttributeValue{"pk": strAV(pk), "sk": strAV(sk)}
}

// randHex returns n random bytes hex-encoded (2n chars). Used to disambiguate
// same-nanosecond event SKs and to keep generated ids collision-resistant across
// concurrent creators (the in-memory store relies on a single lock instead).
func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func genID(prefix string) string {
	return fmt.Sprintf("%s-%d-%s", prefix, time.Now().UnixNano(), randHex(4))
}

// marshal/unmarshal use the domain types' JSON tags as DynamoDB attribute names,
// so a stored item's attributes line up with the on-wire JSON (userId, parentId,
// …) and no parallel dynamodbav tag set has to be maintained.
func marshal(v any) (map[string]types.AttributeValue, error) {
	return attributevalue.MarshalMapWithOptions(v, func(o *attributevalue.EncoderOptions) { o.TagKey = "json" })
}

func unmarshal(m map[string]types.AttributeValue, out any) error {
	return attributevalue.UnmarshalMapWithOptions(m, out, func(o *attributevalue.DecoderOptions) { o.TagKey = "json" })
}

// queryAll drains a Query across pages into a flat slice of items.
func (s *Store) queryAll(ctx context.Context, in *dynamodb.QueryInput) ([]map[string]types.AttributeValue, error) {
	var items []map[string]types.AttributeValue
	p := dynamodb.NewQueryPaginator(s.db, in)
	for p.HasMorePages() {
		out, err := p.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		items = append(items, out.Items...)
	}
	return items, nil
}

func isCondFailed(err error) bool {
	var cce *types.ConditionalCheckFailedException
	return errors.As(err, &cce)
}

// txnCancelHas reports whether a TransactWriteItems error carries a cancellation
// reason with the given code (e.g. "ConditionalCheckFailed", "TransactionConflict").
func txnCancelHas(err error, code string) bool {
	var tce *types.TransactionCanceledException
	if errors.As(err, &tce) {
		for _, r := range tce.CancellationReasons {
			if r.Code != nil && *r.Code == code {
				return true
			}
		}
	}
	return false
}

// isTxnCondFailed reports that a Put's condition (attribute_not_exists) was
// violated — a competing open marker already exists — so the caller should
// re-read and dedupe rather than error.
func isTxnCondFailed(err error) bool { return txnCancelHas(err, "ConditionalCheckFailed") }

// isTxnConflict reports a TransactionConflict cancellation: real DynamoDB
// frequently cancels one of two transactions racing on the SAME item this way,
// and the SDK does NOT auto-retry it. It is transient contention, so the caller
// should retry — not surface it as a hard error (dynamodb-local never emits it,
// so this path rests on code-level reasoning).
func isTxnConflict(err error) bool { return txnCancelHas(err, "TransactionConflict") }

// txnConflictBudget bounds how many times a TransactWriteItems is retried on a
// transient TransactionConflict before the last error is surfaced.
const txnConflictBudget = 5

// retryOnTxnConflict runs fn, retrying ONLY on a transient TransactionConflict
// cancellation with the same bounded, incremental backoff the create path uses.
// Any other error — including a ConditionalCheckFailed the caller must interpret
// itself — and success return immediately. On budget exhaustion it returns fn's
// last (still-conflict) error, so the caller behaves exactly as before this
// retry existed. Extracted so the approve path reuses the create path's conflict
// handling instead of being the one transaction with no retry at all.
func retryOnTxnConflict(fn func() error) error {
	var err error
	for attempt := 0; attempt < txnConflictBudget; attempt++ {
		err = fn()
		if err == nil || !isTxnConflict(err) {
			return err
		}
		time.Sleep(time.Duration(attempt+1) * 20 * time.Millisecond)
	}
	return err
}

// ---- item shapes ------------------------------------------------------------

// Templates, agents, and events nest their domain object under a named attribute
// so pk/sk/gsi keys never collide with a domain field. Consent and consent
// requests instead store their fields at the TOP level, because their writes use
// attribute-level conditions (if_not_exists on the consent id; status = pending
// on resolve) that must address individual domain attributes.

type templateItem struct {
	PK     string                 `json:"pk"`
	SK     string                 `json:"sk"`
	GSI1PK string                 `json:"gsi1pk"`
	GSI1SK string                 `json:"gsi1sk"`
	Tpl    registry.AgentTemplate `json:"tpl"`
}

type agentItem struct {
	PK     string               `json:"pk"`
	SK     string               `json:"sk"`
	GSI1PK string               `json:"gsi1pk"`
	GSI1SK string               `json:"gsi1sk"`
	GSI2PK string               `json:"gsi2pk,omitempty"` // PARENT#<parent>; sparse — omitted for roots
	Agent  registry.AgentRecord `json:"agent"`
}

type eventItem struct {
	PK    string       `json:"pk"`
	SK    string       `json:"sk"`
	Event events.Event `json:"event"`
}

// orgItem nests the Organization under a named attribute so pk/sk/gsi keys
// never collide with a domain field (same shape as templates/agents).
type orgItem struct {
	PK     string                `json:"pk"`
	SK     string                `json:"sk"`
	GSI1PK string                `json:"gsi1pk"`
	GSI1SK string                `json:"gsi1sk"`
	Org    registry.Organization `json:"org"`
}

// ---- templates --------------------------------------------------------------

func (s *Store) PutTemplate(ctx context.Context, t registry.AgentTemplate) error {
	item, err := marshal(templateItem{
		PK: tplPK(t.AgentType), SK: skMeta,
		GSI1PK: gsi1Template, GSI1SK: t.AgentType, Tpl: t,
	})
	if err != nil {
		return err
	}
	_, err = s.db.PutItem(ctx, &dynamodb.PutItemInput{TableName: &s.table, Item: item})
	return err
}

func (s *Store) GetTemplate(ctx context.Context, agentType string) (registry.AgentTemplate, bool, error) {
	out, err := s.db.GetItem(ctx, &dynamodb.GetItemInput{TableName: &s.table, Key: keyOf(tplPK(agentType), skMeta)})
	if err != nil {
		return registry.AgentTemplate{}, false, err
	}
	if out.Item == nil {
		return registry.AgentTemplate{}, false, nil
	}
	var it templateItem
	if err := unmarshal(out.Item, &it); err != nil {
		return registry.AgentTemplate{}, false, err
	}
	return it.Tpl, true, nil
}

// listTemplateItems queries the TPL partition of GSI1 and returns the templates.
func (s *Store) listTemplateItems(ctx context.Context) ([]registry.AgentTemplate, error) {
	items, err := s.queryAll(ctx, &dynamodb.QueryInput{
		TableName:                 &s.table,
		IndexName:                 aws.String("gsi1"),
		KeyConditionExpression:    aws.String("gsi1pk = :pk"),
		ExpressionAttributeValues: map[string]types.AttributeValue{":pk": strAV(gsi1Template)},
	})
	if err != nil {
		return nil, err
	}
	out := make([]registry.AgentTemplate, 0, len(items))
	for _, m := range items {
		var it templateItem
		if err := unmarshal(m, &it); err != nil {
			return nil, err
		}
		out = append(out, it.Tpl)
	}
	return out, nil
}

// ListTemplateTypes returns the spawnable type names, EXCLUDING disabled ones.
func (s *Store) ListTemplateTypes(ctx context.Context) ([]string, error) {
	tpls, err := s.listTemplateItems(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(tpls))
	for _, t := range tpls {
		if t.Status == registry.TemplateStatusDisabled {
			continue
		}
		out = append(out, t.AgentType)
	}
	return out, nil
}

// ListTemplates returns full template records; disabled ones only when asked.
func (s *Store) ListTemplates(ctx context.Context, includeDisabled bool) ([]registry.AgentTemplate, error) {
	tpls, err := s.listTemplateItems(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]registry.AgentTemplate, 0, len(tpls))
	for _, t := range tpls {
		if !includeDisabled && t.Status == registry.TemplateStatusDisabled {
			continue
		}
		out = append(out, t)
	}
	return out, nil
}

// UpdateTemplateStatus sets Status on an existing template with a conditional
// nested SET (guarded by attribute_exists(pk)) — atomic and clobber-free, the
// same shape as UpdateAgentStatus — returning the updated template and whether it
// existed. "tpl" is not reserved; "status" is, so it is aliased.
func (s *Store) UpdateTemplateStatus(ctx context.Context, agentType, status string) (registry.AgentTemplate, bool, error) {
	out, err := s.db.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName:                 &s.table,
		Key:                       keyOf(tplPK(agentType), skMeta),
		UpdateExpression:          aws.String("SET tpl.#s = :st"),
		ConditionExpression:       aws.String("attribute_exists(pk)"),
		ExpressionAttributeNames:  map[string]string{"#s": "status"},
		ExpressionAttributeValues: map[string]types.AttributeValue{":st": strAV(status)},
		ReturnValues:              types.ReturnValueAllNew,
	})
	if err != nil {
		if isCondFailed(err) {
			return registry.AgentTemplate{}, false, nil
		}
		return registry.AgentTemplate{}, false, err
	}
	var it templateItem
	if err := unmarshal(out.Attributes, &it); err != nil {
		return registry.AgentTemplate{}, false, err
	}
	return it.Tpl, true, nil
}

// DeleteTemplate removes a template, reporting whether one existed (via ALL_OLD).
func (s *Store) DeleteTemplate(ctx context.Context, agentType string) (bool, error) {
	out, err := s.db.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName:    &s.table,
		Key:          keyOf(tplPK(agentType), skMeta),
		ReturnValues: types.ReturnValueAllOld,
	})
	if err != nil {
		return false, err
	}
	return len(out.Attributes) > 0, nil
}

// ---- agents -----------------------------------------------------------------

func (s *Store) RegisterAgent(ctx context.Context, r registry.AgentRecord) error {
	it := agentItem{
		PK: agPK(r.AgentID), SK: skMeta,
		GSI1PK: gsi1Agent, GSI1SK: r.AgentID, Agent: r,
	}
	// Sparse GSI2: only children carry PARENT#<parent>, so ListChildren queries
	// exactly the direct children and roots never appear in the index.
	if r.ParentID != "" {
		it.GSI2PK = "PARENT#" + r.ParentID
	}
	item, err := marshal(it)
	if err != nil {
		return err
	}
	_, err = s.db.PutItem(ctx, &dynamodb.PutItemInput{TableName: &s.table, Item: item})
	return err
}

// GetAgent returns the zero AgentRecord (AgentID == "") with no error for an
// unknown id — matching the in-memory contract the handlers rely on.
func (s *Store) GetAgent(ctx context.Context, id string) (registry.AgentRecord, error) {
	out, err := s.db.GetItem(ctx, &dynamodb.GetItemInput{TableName: &s.table, Key: keyOf(agPK(id), skMeta)})
	if err != nil {
		return registry.AgentRecord{}, err
	}
	if out.Item == nil {
		return registry.AgentRecord{}, nil
	}
	var it agentItem
	if err := unmarshal(out.Item, &it); err != nil {
		return registry.AgentRecord{}, err
	}
	return it.Agent, nil
}

// ListAgents returns all agent records EXCLUDING dismissed ones.
func (s *Store) ListAgents(ctx context.Context) ([]registry.AgentRecord, error) {
	items, err := s.queryAll(ctx, &dynamodb.QueryInput{
		TableName:                 &s.table,
		IndexName:                 aws.String("gsi1"),
		KeyConditionExpression:    aws.String("gsi1pk = :pk"),
		ExpressionAttributeValues: map[string]types.AttributeValue{":pk": strAV(gsi1Agent)},
	})
	if err != nil {
		return nil, err
	}
	out := make([]registry.AgentRecord, 0, len(items))
	for _, m := range items {
		var it agentItem
		if err := unmarshal(m, &it); err != nil {
			return nil, err
		}
		if it.Agent.Dismissed {
			continue
		}
		out = append(out, it.Agent)
	}
	return out, nil
}

// DismissAgent flips the nested agent.dismissed flag with a conditional update so
// it is atomic and preserves the sparse GSI2PK; a missing agent reports found=false.
func (s *Store) DismissAgent(ctx context.Context, id string) (bool, error) {
	// #a aliases "agent" — a DynamoDB reserved word — in the nested path.
	return s.condSet(ctx, id, "SET #a.dismissed = :t", map[string]string{"#a": "agent"},
		map[string]types.AttributeValue{":t": &types.AttributeValueMemberBOOL{Value: true}})
}

// UpdateAgentStatus sets the nested agent.status; missing agent → found=false.
func (s *Store) UpdateAgentStatus(ctx context.Context, id, status string) (bool, error) {
	// Both "agent" and "status" are DynamoDB reserved words, so both are aliased.
	return s.condSet(ctx, id, "SET #a.#s = :st",
		map[string]string{"#a": "agent", "#s": "status"},
		map[string]types.AttributeValue{":st": strAV(status)})
}

// condSet applies an UpdateExpression to an agent item guarded by
// attribute_exists(pk), translating a ConditionalCheckFailed into found=false —
// the atomic equivalent of the in-memory "look it up, mutate if present".
func (s *Store) condSet(ctx context.Context, id, expr string, names map[string]string, vals map[string]types.AttributeValue) (bool, error) {
	in := &dynamodb.UpdateItemInput{
		TableName:                 &s.table,
		Key:                       keyOf(agPK(id), skMeta),
		UpdateExpression:          aws.String(expr),
		ConditionExpression:       aws.String("attribute_exists(pk)"),
		ExpressionAttributeValues: vals,
	}
	if len(names) > 0 {
		in.ExpressionAttributeNames = names
	}
	if _, err := s.db.UpdateItem(ctx, in); err != nil {
		if isCondFailed(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// ListChildren returns the direct children of parentID via the sparse GSI2.
//
// GSI reads are ALWAYS eventually consistent, so a child registered microseconds
// earlier can be briefly absent here. The cascade traversal (subtree/depth) that
// consumes this tolerates it: a just-spawned grandchild not yet visible is one a
// revoke need not reach (it has no live authority yet), and the operator's
// stale-sweep + the spawn-time depth check backstop the window. Accepted at beta
// scale; documented so it is a decision, not an oversight.
func (s *Store) ListChildren(ctx context.Context, parentID string) ([]registry.AgentRecord, error) {
	items, err := s.queryAll(ctx, &dynamodb.QueryInput{
		TableName:                 &s.table,
		IndexName:                 aws.String("gsi2"),
		KeyConditionExpression:    aws.String("gsi2pk = :pk"),
		ExpressionAttributeValues: map[string]types.AttributeValue{":pk": strAV("PARENT#" + parentID)},
	})
	if err != nil {
		return nil, err
	}
	out := make([]registry.AgentRecord, 0, len(items))
	for _, m := range items {
		var it agentItem
		if err := unmarshal(m, &it); err != nil {
			return nil, err
		}
		out = append(out, it.Agent)
	}
	return out, nil
}

// ---- organizations ----------------------------------------------------------

// PutOrg upserts an org record (a plain PutItem replaces the whole item,
// mirroring the in-memory map assignment and PutTemplate's overwrite).
func (s *Store) PutOrg(ctx context.Context, org registry.Organization) error {
	item, err := marshal(orgItem{
		PK: orgPK(org.OrgID), SK: skMeta,
		GSI1PK: gsi1Org, GSI1SK: org.OrgID, Org: org,
	})
	if err != nil {
		return err
	}
	_, err = s.db.PutItem(ctx, &dynamodb.PutItemInput{TableName: &s.table, Item: item})
	return err
}

// GetOrg returns the org and whether it exists; an unknown id is (zero, false).
func (s *Store) GetOrg(ctx context.Context, id string) (registry.Organization, bool, error) {
	out, err := s.db.GetItem(ctx, &dynamodb.GetItemInput{TableName: &s.table, Key: keyOf(orgPK(id), skMeta)})
	if err != nil {
		return registry.Organization{}, false, err
	}
	if out.Item == nil {
		return registry.Organization{}, false, nil
	}
	var it orgItem
	if err := unmarshal(out.Item, &it); err != nil {
		return registry.Organization{}, false, err
	}
	return it.Org, true, nil
}

// ListOrgs returns every org record via the ORG partition of GSI1.
func (s *Store) ListOrgs(ctx context.Context) ([]registry.Organization, error) {
	items, err := s.queryAll(ctx, &dynamodb.QueryInput{
		TableName:                 &s.table,
		IndexName:                 aws.String("gsi1"),
		KeyConditionExpression:    aws.String("gsi1pk = :pk"),
		ExpressionAttributeValues: map[string]types.AttributeValue{":pk": strAV(gsi1Org)},
	})
	if err != nil {
		return nil, err
	}
	out := make([]registry.Organization, 0, len(items))
	for _, m := range items {
		var it orgItem
		if err := unmarshal(m, &it); err != nil {
			return nil, err
		}
		out = append(out, it.Org)
	}
	return out, nil
}

// DeleteOrg removes an org, reporting whether one existed (via ALL_OLD) —
// matching DeleteTemplate's found-ness contract.
func (s *Store) DeleteOrg(ctx context.Context, id string) (bool, error) {
	out, err := s.db.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName:    &s.table,
		Key:          keyOf(orgPK(id), skMeta),
		ReturnValues: types.ReturnValueAllOld,
	})
	if err != nil {
		return false, err
	}
	return len(out.Attributes) > 0, nil
}

// ---- events -----------------------------------------------------------------

// AppendEvent stamps the event's ID + Timestamp and stores it under an SK that
// sorts by append order. Returns the stored copy.
//
// Ordering contract (shared with the in-memory store, and what GetEvents
// guarantees): events are returned oldest-first. The event ID is
// %020d-nano-plus-random-suffix — genuinely unique (the suffix disambiguates two
// appends in the same nanosecond) AND ordering-stable (the zero-padded nano sorts
// chronologically). Critically, the ID reuses the SAME nano + suffix baked into
// the sort key, so a consumer that sorts by ID and the SK-ordered GetEvents agree
// on order, and matches the in-memory store's eventID() format byte-for-byte. The
// dashboard's incremental renderer keys off this id, so uniqueness is load-bearing.
func (s *Store) AppendEvent(ctx context.Context, agentID string, e events.Event) (events.Event, error) {
	now := time.Now()
	nano := now.UnixNano()
	// One suffix, used in BOTH the id and the SK, so id order == SK order.
	suffix := randHex(4)
	e.ID = fmt.Sprintf("%020d-%s", nano, suffix)
	e.Timestamp = now
	// %020d zero-pads the nanosecond so lexical SK order == chronological order;
	// the shared random suffix disambiguates two appends in the same nanosecond.
	sk := fmt.Sprintf("EV#%020d#%s", nano, suffix)
	item, err := marshal(eventItem{PK: agPK(agentID), SK: sk, Event: e})
	if err != nil {
		return events.Event{}, err
	}
	if _, err := s.db.PutItem(ctx, &dynamodb.PutItemInput{TableName: &s.table, Item: item}); err != nil {
		return events.Event{}, err
	}
	return e, nil
}

// GetEvents returns an agent's events in append order.
func (s *Store) GetEvents(ctx context.Context, agentID string) ([]events.Event, error) {
	items, err := s.queryAll(ctx, &dynamodb.QueryInput{
		TableName:              &s.table,
		KeyConditionExpression: aws.String("pk = :pk AND begins_with(sk, :ev)"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":pk": strAV(agPK(agentID)),
			":ev": strAV("EV#"),
		},
		// Ascending SK == chronological order (default, stated for clarity).
		ScanIndexForward: aws.Bool(true),
	})
	if err != nil {
		return nil, err
	}
	out := make([]events.Event, 0, len(items))
	for _, m := range items {
		var it eventItem
		if err := unmarshal(m, &it); err != nil {
			return nil, err
		}
		out = append(out, it.Event)
	}
	return out, nil
}

// ---- consents ---------------------------------------------------------------

// consentUpsertUpdate builds the shared UpdateExpression that writes a fresh grant
// for its edge while keeping a STABLE consent id (and its CONID# GSI2 key) across
// re-grants via if_not_exists, overwriting the grant fields and clearing revoked.
// A brand-new candidate id is minted every call but only adopted when the item
// does not yet exist. It is used both by UpsertConsent (a standalone UpdateItem)
// and by the approve path (one item of a TransactWriteItems), so the grant write
// is byte-identical whichever way it happens.
func consentUpsertUpdate(rec registry.ConsentRecord) (edge, expr string, names map[string]string, vals map[string]types.AttributeValue, err error) {
	cand := genID("consent")
	edge = edgeKey(rec.UserID, rec.ParentType, rec.ChildType)
	scopes := rec.Scopes
	if scopes == nil {
		scopes = []string{}
	}
	scopesAV, err := attributevalue.Marshal(scopes)
	if err != nil {
		return "", "", nil, nil, err
	}
	vals = map[string]types.AttributeValue{
		":id":   strAV(cand),
		":g2":   strAV("CONID#" + cand),
		":g1pk": strAV(gsi1Consent),
		":g1sk": strAV(edge),
		":u":    strAV(rec.UserID),
		":p":    strAV(rec.ParentType),
		":c":    strAV(rec.ChildType),
		":sc":   scopesAV,
		":ga":   timeAV(rec.GrantedAt),
		":rev":  &types.AttributeValueMemberBOOL{Value: false},
	}
	// id + gsi2pk use if_not_exists so re-grants keep the original id (and its
	// CONID# lookup key); everything else is overwritten.
	expr = "SET #id = if_not_exists(#id, :id), gsi2pk = if_not_exists(gsi2pk, :g2), " +
		"gsi1pk = :g1pk, gsi1sk = :g1sk, userId = :u, parentType = :p, childType = :c, " +
		"scopes = :sc, grantedAt = :ga, revoked = :rev"
	if rec.ExpiresAt != nil {
		vals[":ea"] = timeAV(*rec.ExpiresAt)
		expr += ", expiresAt = :ea"
	} else {
		// A fresh non-expiring grant must not inherit a prior expiresAt.
		expr += " REMOVE expiresAt"
	}
	names = map[string]string{"#id": "id"}
	return edge, expr, names, vals, nil
}

// UpsertConsent stores a fresh grant for its edge (see consentUpsertUpdate),
// returning the stored record with its stable id.
func (s *Store) UpsertConsent(ctx context.Context, rec registry.ConsentRecord) (registry.ConsentRecord, error) {
	edge, expr, names, vals, err := consentUpsertUpdate(rec)
	if err != nil {
		return registry.ConsentRecord{}, err
	}
	out, err := s.db.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName:                 &s.table,
		Key:                       keyOf(conPK(edge), skMeta),
		UpdateExpression:          aws.String(expr),
		ExpressionAttributeNames:  names,
		ExpressionAttributeValues: vals,
		ReturnValues:              types.ReturnValueAllNew,
	})
	if err != nil {
		return registry.ConsentRecord{}, err
	}
	var got registry.ConsentRecord
	if err := unmarshal(out.Attributes, &got); err != nil {
		return registry.ConsentRecord{}, err
	}
	return got, nil
}

func (s *Store) FindConsent(ctx context.Context, userID, parentType, childType string) (registry.ConsentRecord, bool, error) {
	// Strongly consistent: FindConsent gates the auto-approve/short-circuit
	// decision (GET /v1/consents/check and the create-consent-request path), and a
	// just-written grant or revoke MUST be honoured immediately — an eventually
	// consistent read could re-prompt on an existing grant or approve on a revoked
	// one, and it also lets the create path reliably absorb a sibling the GSI-based
	// approve-sweep might still be lagging on.
	out, err := s.db.GetItem(ctx, &dynamodb.GetItemInput{
		TableName:      &s.table,
		Key:            keyOf(conPK(edgeKey(userID, parentType, childType)), skMeta),
		ConsistentRead: aws.Bool(true),
	})
	if err != nil {
		return registry.ConsentRecord{}, false, err
	}
	if out.Item == nil {
		return registry.ConsentRecord{}, false, nil
	}
	var rec registry.ConsentRecord
	if err := unmarshal(out.Item, &rec); err != nil {
		return registry.ConsentRecord{}, false, err
	}
	return rec, true, nil
}

// ListConsents returns all consents, or one user's when userID is non-empty
// (begins_with on the user-prefixed edge). Revoked records are included.
func (s *Store) ListConsents(ctx context.Context, userID string) ([]registry.ConsentRecord, error) {
	in := &dynamodb.QueryInput{
		TableName:                 &s.table,
		IndexName:                 aws.String("gsi1"),
		KeyConditionExpression:    aws.String("gsi1pk = :pk"),
		ExpressionAttributeValues: map[string]types.AttributeValue{":pk": strAV(gsi1Consent)},
	}
	if userID != "" {
		in.KeyConditionExpression = aws.String("gsi1pk = :pk AND begins_with(gsi1sk, :u)")
		in.ExpressionAttributeValues[":u"] = strAV(userID + "|")
	}
	items, err := s.queryAll(ctx, in)
	if err != nil {
		return nil, err
	}
	out := make([]registry.ConsentRecord, 0, len(items))
	for _, m := range items {
		var rec registry.ConsentRecord
		if err := unmarshal(m, &rec); err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, nil
}

// consentRow carries the pk/sk alongside the record so a GSI2 (CONID#) lookup can
// address the base item for a conditional revoke write.
type consentRow struct {
	PK string `json:"pk"`
	SK string `json:"sk"`
	registry.ConsentRecord
}

// RevokeConsent marks the consent with the given id revoked, scoped to userID
// when non-empty (a matching id owned by another user is treated as not found —
// exactly the in-memory rule that stops cross-user revocation).
func (s *Store) RevokeConsent(ctx context.Context, id, userID string) (bool, error) {
	// The id -> consent lookup is via GSI2 (CONID#), which is ALWAYS eventually
	// consistent: a consent the API just handed the caller can be momentarily
	// absent from the index. A genuinely-unknown id and index-lag both present as
	// an empty result, so retry a few times with a short backoff before concluding
	// "not found" — bounded, and only pays the cost on the empty path.
	var items []map[string]types.AttributeValue
	for attempt := 0; attempt < 4; attempt++ {
		var err error
		items, err = s.queryAll(ctx, &dynamodb.QueryInput{
			TableName:                 &s.table,
			IndexName:                 aws.String("gsi2"),
			KeyConditionExpression:    aws.String("gsi2pk = :pk"),
			ExpressionAttributeValues: map[string]types.AttributeValue{":pk": strAV("CONID#" + id)},
		})
		if err != nil {
			return false, err
		}
		if len(items) > 0 {
			break
		}
		if attempt < 3 {
			time.Sleep(100 * time.Millisecond)
		}
	}
	if len(items) == 0 {
		return false, nil
	}
	var row consentRow
	if err := unmarshal(items[0], &row); err != nil {
		return false, err
	}
	if userID != "" && row.UserID != userID {
		return false, nil
	}
	if _, err := s.db.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName:                 &s.table,
		Key:                       keyOf(row.PK, row.SK),
		UpdateExpression:          aws.String("SET revoked = :t"),
		ExpressionAttributeValues: map[string]types.AttributeValue{":t": &types.AttributeValueMemberBOOL{Value: true}},
	}); err != nil {
		return false, err
	}
	return true, nil
}

// ConsentExpiry derives a fresh grant's expiry from the parent template's
// per-child consentTTL — identical policy to the in-memory store, reading the
// template through GetTemplate. Nil means never expires (also the fallback for a
// missing template/child or an unparsable TTL).
func (s *Store) ConsentExpiry(ctx context.Context, parentType, childType string, from time.Time) (*time.Time, error) {
	tpl, ok, err := s.GetTemplate(ctx, parentType)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}
	cp, ok := tpl.Delegation.ChildPolicies[childType]
	if !ok || cp.ConsentTTL == "" {
		return nil, nil
	}
	d, err := time.ParseDuration(cp.ConsentTTL)
	if err != nil {
		log.Printf("template %s: bad consentTTL %q for child %s: %v", parentType, cp.ConsentTTL, childType, err)
		return nil, nil
	}
	t := from.Add(d)
	return &t, nil
}

// ---- consent requests -------------------------------------------------------

// creqItem marshals a consent request to a top-level item map plus its keys.
func creqItem(cr registry.ConsentRequest) (map[string]types.AttributeValue, error) {
	m, err := marshal(cr)
	if err != nil {
		return nil, err
	}
	m["pk"] = strAV(creqPK(cr.ID))
	m["sk"] = strAV(skMeta)
	m["gsi1pk"] = strAV(gsi1Creq)
	m["gsi1sk"] = strAV(cr.UserID + "#" + cr.CreatedAt.Format(time.RFC3339Nano) + "#" + cr.ID)
	m["gsi2pk"] = strAV("EDGE#" + edgeKey(cr.UserID, cr.ParentType, cr.ChildType))
	return m, nil
}

// CreateConsentRequest atomically claims the open (pending) slot for an edge/agent
// via a two-item transaction: Put the OPEN#<key> marker guarded by
// attribute_not_exists(pk) together with the CREQ#<id> request. If the marker
// already exists the request is a duplicate — read it back and return
// (existing, false), the same idempotent re-notify the in-memory store gives.
func (s *Store) CreateConsentRequest(ctx context.Context, req registry.ConsentRequest) (registry.ConsentRequest, bool, error) {
	key := openRequestKey(req.UserID, req.ParentType, req.ChildType, req.AgentID)
	// A small retry budget (shared txnConflictBudget, so the create and approve
	// paths retry conflicts to the same depth): each iteration either wins, dedupes
	// to an existing pending request, cleans one stale marker and retries, or backs
	// off a TransactionConflict and retries. A handful of attempts covers real
	// contention without spinning.
	for attempt := 0; attempt < txnConflictBudget; attempt++ {
		fresh := req
		if fresh.Scopes == nil {
			fresh.Scopes = []string{}
		}
		fresh.ID = genID("creq")
		fresh.Status = registry.ConsentPending
		fresh.CreatedAt = time.Now()

		item, err := creqItem(fresh)
		if err != nil {
			return registry.ConsentRequest{}, false, err
		}
		openItem := map[string]types.AttributeValue{
			"pk": strAV(openPK(key)), "sk": strAV(skMeta), "requestId": strAV(fresh.ID),
		}
		_, err = s.db.TransactWriteItems(ctx, &dynamodb.TransactWriteItemsInput{
			TransactItems: []types.TransactWriteItem{
				{Put: &types.Put{
					TableName:           &s.table,
					Item:                openItem,
					ConditionExpression: aws.String("attribute_not_exists(pk)"),
				}},
				{Put: &types.Put{TableName: &s.table, Item: item}},
			},
		})
		if err == nil {
			return fresh, true, nil
		}
		switch {
		case isTxnCondFailed(err):
			// Marker already present: a request already owns this open slot.
			existing, markerReqID, ok, rerr := s.readOpenRequest(ctx, key)
			if rerr != nil {
				return registry.ConsentRequest{}, false, rerr
			}
			if ok && existing.Status == registry.ConsentPending {
				return existing, false, nil
			}
			// Stale marker (request gone or already resolved). Remove it — but only
			// if it still points at the same stale id, so we never delete a marker a
			// concurrent creator has since refreshed — then retry.
			if err := s.deleteOpenMarkerIf(ctx, key, markerReqID); err != nil {
				return registry.ConsentRequest{}, false, err
			}
		case isTxnConflict(err):
			// Transient same-item contention: back off briefly and retry.
			time.Sleep(time.Duration(attempt+1) * 20 * time.Millisecond)
		default:
			return registry.ConsentRequest{}, false, err
		}
	}
	return registry.ConsentRequest{}, false, fmt.Errorf("create consent request: persistent contention on %q", key)
}

// readOpenRequest resolves an OPEN#<key> marker to the request it points at,
// returning the request, the marker's requestId, and whether both were found.
//
// Both reads are STRONGLY CONSISTENT: this runs only after our own transaction
// lost the attribute_not_exists race, so the marker is already committed — an
// eventually-consistent read that returned a stale nil for the marker, or a stale
// nil for the still-pending request it points at, would send the caller down the
// "stale marker" branch and delete a VALID open marker, splitting one edge into
// two live pending requests (finding 1/6).
func (s *Store) readOpenRequest(ctx context.Context, key string) (registry.ConsentRequest, string, bool, error) {
	mk, err := s.db.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: &s.table, Key: keyOf(openPK(key), skMeta), ConsistentRead: aws.Bool(true),
	})
	if err != nil {
		return registry.ConsentRequest{}, "", false, err
	}
	if mk.Item == nil {
		return registry.ConsentRequest{}, "", false, nil
	}
	var marker struct {
		RequestID string `json:"requestId"`
	}
	if err := unmarshal(mk.Item, &marker); err != nil {
		return registry.ConsentRequest{}, "", false, err
	}
	cr, ok, err := s.getConsentRequest(ctx, marker.RequestID, true)
	if err != nil {
		return registry.ConsentRequest{}, marker.RequestID, false, err
	}
	return cr, marker.RequestID, ok, nil
}

// deleteOpenMarkerIf removes an OPEN#<key> marker only if it still points at
// expectID (conditional), so a concurrent refresh is never clobbered — the fix
// for the marker-clobber race (finding 2): a delayed resolver must not delete a
// marker a newer request has since installed.
func (s *Store) deleteOpenMarkerIf(ctx context.Context, key, expectID string) error {
	_, err := s.db.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName:                 &s.table,
		Key:                       keyOf(openPK(key), skMeta),
		ConditionExpression:       aws.String("requestId = :rid"),
		ExpressionAttributeValues: map[string]types.AttributeValue{":rid": strAV(expectID)},
	})
	if err != nil && !isCondFailed(err) {
		return err
	}
	return nil // a lost condition means someone else already moved the marker on
}

// freeOpenMarker is the best-effort, id-guarded marker removal used after a
// resolve/sweep: it only clears the marker if it still points at expectID, and
// logs (never surfaces) any hard error.
func (s *Store) freeOpenMarker(ctx context.Context, key, expectID string) {
	if err := s.deleteOpenMarkerIf(ctx, key, expectID); err != nil {
		log.Printf("dynamostore: free open marker %q: %v", key, err)
	}
}

// CreateApprovedConsentRequest stores a request already resolved as approved — no
// open marker, since nothing is pending.
func (s *Store) CreateApprovedConsentRequest(ctx context.Context, req registry.ConsentRequest) (registry.ConsentRequest, error) {
	now := time.Now()
	if req.Scopes == nil {
		req.Scopes = []string{}
	}
	req.ID = genID("creq")
	req.Status = registry.ConsentApproved
	req.CreatedAt = now
	req.ResolvedAt = &now
	item, err := creqItem(req)
	if err != nil {
		return registry.ConsentRequest{}, err
	}
	if _, err := s.db.PutItem(ctx, &dynamodb.PutItemInput{TableName: &s.table, Item: item}); err != nil {
		return registry.ConsentRequest{}, err
	}
	return req, nil
}

func (s *Store) GetConsentRequest(ctx context.Context, id string) (registry.ConsentRequest, bool, error) {
	// Public reads are eventually consistent (cheaper, and the poller re-reads):
	// the internal recovery paths that must not see a stale replica pass true.
	return s.getConsentRequest(ctx, id, false)
}

// getConsentRequest reads a request by id, optionally with a strongly-consistent
// read. Strong reads are reserved for the invariant-critical recovery paths
// (create-race marker resolution, resolve-conflict recovery) where an
// eventually-consistent miss would corrupt the one-open-request invariant or
// return a spurious 404 for an already-resolved request.
func (s *Store) getConsentRequest(ctx context.Context, id string, consistent bool) (registry.ConsentRequest, bool, error) {
	out, err := s.db.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: &s.table, Key: keyOf(creqPK(id), skMeta), ConsistentRead: aws.Bool(consistent),
	})
	if err != nil {
		return registry.ConsentRequest{}, false, err
	}
	if out.Item == nil {
		return registry.ConsentRequest{}, false, nil
	}
	var cr registry.ConsentRequest
	if err := unmarshal(out.Item, &cr); err != nil {
		return registry.ConsentRequest{}, false, err
	}
	return cr, true, nil
}

// ListConsentRequests returns brokered requests, optionally scoped to userID
// (begins_with on the user-prefixed GSI1 SK) and/or filtered by status in Go.
func (s *Store) ListConsentRequests(ctx context.Context, userID, status string) ([]registry.ConsentRequest, error) {
	in := &dynamodb.QueryInput{
		TableName:                 &s.table,
		IndexName:                 aws.String("gsi1"),
		KeyConditionExpression:    aws.String("gsi1pk = :pk"),
		ExpressionAttributeValues: map[string]types.AttributeValue{":pk": strAV(gsi1Creq)},
	}
	if userID != "" {
		in.KeyConditionExpression = aws.String("gsi1pk = :pk AND begins_with(gsi1sk, :u)")
		in.ExpressionAttributeValues[":u"] = strAV(userID + "#")
	}
	items, err := s.queryAll(ctx, in)
	if err != nil {
		return nil, err
	}
	out := make([]registry.ConsentRequest, 0, len(items))
	for _, m := range items {
		var cr registry.ConsentRequest
		if err := unmarshal(m, &cr); err != nil {
			return nil, err
		}
		if status != "" && string(cr.Status) != status {
			continue
		}
		out = append(out, cr)
	}
	return out, nil
}

// requestsByEdge returns every consent request on an edge (for the approve-sweep).
func (s *Store) requestsByEdge(ctx context.Context, edge string) ([]registry.ConsentRequest, error) {
	items, err := s.queryAll(ctx, &dynamodb.QueryInput{
		TableName:                 &s.table,
		IndexName:                 aws.String("gsi2"),
		KeyConditionExpression:    aws.String("gsi2pk = :pk"),
		ExpressionAttributeValues: map[string]types.AttributeValue{":pk": strAV("EDGE#" + edge)},
	})
	if err != nil {
		return nil, err
	}
	out := make([]registry.ConsentRequest, 0, len(items))
	for _, m := range items {
		var cr registry.ConsentRequest
		if err := unmarshal(m, &cr); err != nil {
			return nil, err
		}
		out = append(out, cr)
	}
	return out, nil
}

// ResolveConsentRequest approves or denies a pending request, mirroring the
// in-memory broker: idempotent on already-resolved requests; a non-empty userID
// scopes the resolve to that user's own request; on approve it upserts the edge
// grant then sweeps other pending same-edge requests the grant now covers.
//
// The status pivot and (on approve) the grant upsert commit in ONE
// TransactWriteItems, so there is never an approved-request-without-a-grant
// state. Only the sibling approve-sweep remains best-effort (each flip is an
// independent conditional write) — the same best-effort concurrency intent the
// in-memory sweep documents, now bounded by the marker/grant ordering above.
func (s *Store) ResolveConsentRequest(ctx context.Context, id, userID string, approve bool, scopes []string) (registry.ConsentRequest, bool, error) {
	cr, ok, err := s.GetConsentRequest(ctx, id)
	if err != nil {
		return registry.ConsentRequest{}, false, err
	}
	// Unknown id OR a request owned by a different user is indistinguishable from
	// not-found (the confused-deputy scoping that stops cross-user resolves).
	if !ok || (userID != "" && cr.UserID != userID) {
		return registry.ConsentRequest{}, false, nil
	}

	now := time.Now()
	grantScopes := scopes
	var expiry *time.Time
	if approve {
		if grantScopes == nil {
			grantScopes = cr.Scopes
		}
		expiry, err = s.ConsentExpiry(ctx, cr.ParentType, cr.ChildType, now)
		if err != nil {
			return registry.ConsentRequest{}, false, err
		}
	}

	newStatus := registry.ConsentDenied
	if approve {
		newStatus = registry.ConsentApproved
	}

	// Conditional pivot: flip pending -> approved/denied only if still pending
	// (and, when scoped, still owned by userID). Shared by the deny UpdateItem and
	// the approve transaction below.
	pivotNames := map[string]string{"#s": "status"}
	pivotVals := map[string]types.AttributeValue{
		":pending": strAV(string(registry.ConsentPending)),
		":new":     strAV(string(newStatus)),
		":now":     timeAV(now),
	}
	pivotCond := "#s = :pending"
	if userID != "" {
		pivotNames["#u"] = "userId"
		pivotVals[":u"] = strAV(userID)
		pivotCond += " AND #u = :u"
	}
	pivotUpdate := "SET #s = :new, resolvedAt = :now"

	// The committed request is cr with the new status/resolvedAt — exactly what
	// ReturnValues=ALL_NEW would yield, but built locally so the approve path can
	// use a transaction (which returns no attributes).
	updated := cr
	updated.Status = newStatus
	updated.ResolvedAt = &now

	ownKey := openRequestKey(cr.UserID, cr.ParentType, cr.ChildType, cr.AgentID)

	if !approve {
		// Deny: a single conditional pivot; no grant.
		_, err := s.db.UpdateItem(ctx, &dynamodb.UpdateItemInput{
			TableName:                 &s.table,
			Key:                       keyOf(creqPK(id), skMeta),
			UpdateExpression:          aws.String(pivotUpdate),
			ConditionExpression:       aws.String(pivotCond),
			ExpressionAttributeNames:  pivotNames,
			ExpressionAttributeValues: pivotVals,
		})
		if err != nil {
			if isCondFailed(err) {
				return s.resolveRecover(ctx, id)
			}
			return registry.ConsentRequest{}, false, err
		}
		if s.afterPivot != nil {
			s.afterPivot()
		}
		s.freeOpenMarker(ctx, ownKey, id)
		return updated, true, nil
	}

	// Approve: pivot the request AND upsert the covering grant in ONE
	// TransactWriteItems, so "approved" and its grant are all-or-nothing — there is
	// no approved-but-no-grant window the in-memory single-lock version can't have.
	// The marker delete stays OUT of the transaction on purpose: a conditional
	// marker delete could be cancelled by a concurrent stale-cleanup and would then
	// roll back the whole pivot; instead it is freed conditionally right after
	// (finding 2), and the grant lands first so a fresh create for the edge sees it
	// via the strongly-consistent FindConsent short-circuit (finding 3).
	grantRec := registry.ConsentRecord{
		UserID: cr.UserID, ParentType: cr.ParentType, ChildType: cr.ChildType,
		Scopes: grantScopes, GrantedAt: now, ExpiresAt: expiry,
	}
	gEdge, gExpr, gNames, gVals, err := consentUpsertUpdate(grantRec)
	if err != nil {
		return registry.ConsentRequest{}, false, err
	}
	// Wrapped in the shared TransactionConflict retry (parity with the create
	// path): a concurrent write on the request or grant item can cancel this
	// transaction with a transient TransactionConflict the SDK won't auto-retry.
	// A ConditionalCheckFailed (already-resolved) still falls straight through to
	// the idempotent recovery below.
	err = retryOnTxnConflict(func() error {
		_, e := s.db.TransactWriteItems(ctx, &dynamodb.TransactWriteItemsInput{
			TransactItems: []types.TransactWriteItem{
				{Update: &types.Update{
					TableName:                 &s.table,
					Key:                       keyOf(creqPK(id), skMeta),
					UpdateExpression:          aws.String(pivotUpdate),
					ConditionExpression:       aws.String(pivotCond),
					ExpressionAttributeNames:  pivotNames,
					ExpressionAttributeValues: pivotVals,
				}},
				{Update: &types.Update{
					TableName:                 &s.table,
					Key:                       keyOf(conPK(gEdge), skMeta),
					UpdateExpression:          aws.String(gExpr),
					ExpressionAttributeNames:  gNames,
					ExpressionAttributeValues: gVals,
				}},
			},
		})
		return e
	})
	if err != nil {
		// A failed pivot condition (already resolved) is the idempotent path.
		if isTxnCondFailed(err) || isCondFailed(err) {
			return s.resolveRecover(ctx, id)
		}
		return registry.ConsentRequest{}, false, err
	}

	// Test seam: interleave a concurrent create in the pivot→marker-free window.
	if s.afterPivot != nil {
		s.afterPivot()
	}

	s.freeOpenMarker(ctx, ownKey, id)

	// granted mirrors what the transaction wrote; the sweep's EvaluateConsent needs
	// only the scopes/expiry/revoked, not the stable id.
	granted := grantRec
	granted.Revoked = false
	// The approve-sweep queries GSI2 (always eventually consistent), so a sibling
	// pending request committed microseconds earlier can be missed here and stay
	// pending — the documented best-effort boundary matching the in-memory sweep.
	// It is bounded, not permanent, and two backstops close it WITHOUT relying on
	// this GSI read: (1) the waiting child's next CIBA CreateConsentRequest is
	// short-circuited to approved against the now-strongly-consistent grant (the
	// create handler's FindConsent check), and (2) any read of a still-pending
	// request re-evaluates it against that same grant and resolves it in place —
	// the handler-level backstopPendingConsentRequest in cmd/registry, which runs
	// for both stores so the sweep miss self-heals on the very next poll.
	s.sweep(ctx, id, granted, now)
	return updated, true, nil
}

// ApprovePendingConsentRequest flips a still-pending request to approved with NO
// grant write — the status-only pivot the read-time backstop uses when a covering
// grant already exists. It is the SAME conditional flip the approve-sweep applies
// per sibling (SET status=approved iff still pending), so it can never un-revoke,
// scope-clobber, or re-stamp the TTL of the covering grant. Idempotent: a failed
// condition means the request was already resolved, recovered via a strong re-read.
func (s *Store) ApprovePendingConsentRequest(ctx context.Context, id string) (registry.ConsentRequest, bool, error) {
	cr, ok, err := s.GetConsentRequest(ctx, id)
	if err != nil {
		return registry.ConsentRequest{}, false, err
	}
	if !ok {
		return registry.ConsentRequest{}, false, nil
	}
	if cr.Status != registry.ConsentPending {
		return cr, true, nil // already resolved — idempotent
	}
	now := time.Now()
	_, err = s.db.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName:                &s.table,
		Key:                      keyOf(creqPK(id), skMeta),
		UpdateExpression:         aws.String("SET #s = :app, resolvedAt = :now"),
		ConditionExpression:      aws.String("#s = :pending"),
		ExpressionAttributeNames: map[string]string{"#s": "status"},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":app":     strAV(string(registry.ConsentApproved)),
			":pending": strAV(string(registry.ConsentPending)),
			":now":     timeAV(now),
		},
	})
	if err != nil {
		// A concurrent resolver flipped it first: recover the resolved state.
		if isCondFailed(err) {
			return s.resolveRecover(ctx, id)
		}
		return registry.ConsentRequest{}, false, err
	}
	// Free the open marker, id-guarded, exactly as the sweep does after a flip.
	s.freeOpenMarker(ctx, openRequestKey(cr.UserID, cr.ParentType, cr.ChildType, cr.AgentID), id)
	updated := cr
	updated.Status = registry.ConsentApproved
	updated.ResolvedAt = &now
	return updated, true, nil
}

// resolveRecover is the idempotent recovery for a pivot whose condition failed:
// the request was already resolved, so a STRONGLY consistent re-read (the failed
// condition proves the item was just written) returns it as (request, true);
// only a genuinely-absent/still-pending item is (zero, false).
func (s *Store) resolveRecover(ctx context.Context, id string) (registry.ConsentRequest, bool, error) {
	cur, ok, err := s.getConsentRequest(ctx, id, true)
	if err != nil {
		return registry.ConsentRequest{}, false, err
	}
	if ok && cur.Status != registry.ConsentPending {
		return cur, true, nil
	}
	return registry.ConsentRequest{}, false, nil
}

// sweep auto-approves every other still-pending request on the same edge that the
// fresh grant covers — so agents waiting on the same (user, parentType,
// childType) edge activate without their own prompt. Per-item conditional writes
// keep each flip a no-op if the sibling was concurrently resolved; a request the
// grant does NOT cover (e.g. a wider scope set) stays pending.
func (s *Store) sweep(ctx context.Context, resolvedID string, granted registry.ConsentRecord, now time.Time) {
	edge := edgeKey(granted.UserID, granted.ParentType, granted.ChildType)
	others, err := s.requestsByEdge(ctx, edge)
	if err != nil {
		log.Printf("dynamostore: sweep query edge %q: %v", edge, err)
		return
	}
	for _, other := range others {
		if other.ID == resolvedID || other.Status != registry.ConsentPending {
			continue
		}
		if !registry.EvaluateConsent(granted, other.Scopes, now).Granted {
			continue // wider/uncovered scopes — leave it pending for its own prompt
		}
		_, err := s.db.UpdateItem(ctx, &dynamodb.UpdateItemInput{
			TableName:                &s.table,
			Key:                      keyOf(creqPK(other.ID), skMeta),
			UpdateExpression:         aws.String("SET #s = :app, resolvedAt = :now"),
			ConditionExpression:      aws.String("#s = :pending"),
			ExpressionAttributeNames: map[string]string{"#s": "status"},
			ExpressionAttributeValues: map[string]types.AttributeValue{
				":app":     strAV(string(registry.ConsentApproved)),
				":pending": strAV(string(registry.ConsentPending)),
				":now":     timeAV(now),
			},
		})
		if err != nil {
			if !isCondFailed(err) {
				log.Printf("dynamostore: sweep approve %s: %v", other.ID, err)
			}
			continue
		}
		// Id-guarded so a delayed sweep never clobbers a marker a newer request has
		// since installed for this edge/agent (finding 2).
		s.freeOpenMarker(ctx, openRequestKey(other.UserID, other.ParentType, other.ChildType, other.AgentID), other.ID)
	}
}
