// Package dynamostore is a durable DynamoDB implementation of the
// mobilegateway.DeviceStore contract, used on the AWS deploy where the in-memory
// byUser map would lose every registered device on a pod restart — a beta user's
// phone would then silently stop receiving spawn-consent pushes until it happened
// to re-register.
//
// Dependency direction: this adapter depends on the mobilegateway contract and
// the AWS SDK, never the reverse — no handler knows DynamoDB exists. Backend
// choice is wiring in cmd/mobile-gateway (DEVICE_STORE=dynamodb) + deploy config
// only, so the kind/local path stays byte-for-byte on MemoryDeviceStore.
//
// # Table design
//
// A dedicated table (least-privilege IAM per service — the mobile-gateway role
// can touch only this table, never the registry's) with one item per device:
//
//	pk = USER#<userId>   sk = <deviceId>   attrs: id, platform, pushToken, createdAt
//
// Access patterns: Put = PutItem (upsert by (user,id) — a re-register with a
// rotated push token overwrites in place); Delete = DeleteItem with
// ReturnValues=ALL_OLD so a missing item reports found=false without a prior
// read; ListByUser = Query on pk.
//
// # Key hygiene / cross-user isolation
//
// The partition key is USER#<userId> with a FIXED "USER#" prefix, so isolation is
// STRUCTURAL, not a filter: a Delete keyed by (userId, deviceId) can only ever
// reach the caller's own partition — a cross-user delete is impossible the same
// way the in-memory byUser map makes it impossible (this is what the DeviceStore
// contract promises: "a device owned by another user is reported as not found,
// never deleted"). Both key components are charset-constrained upstream: userId
// is the access token's `sub` (a Keycloak UUID subject, minus any "user:" prefix
// — see mobilegateway.requireToken), never client-supplied; deviceId is a
// server-generated 128-bit hex id (mobilegateway.registerDevice's newID), never
// taken from the request body. So neither can smuggle a "#" or a control byte
// that would let a crafted value straddle partitions or forge the prefix.
package dynamostore

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/spawnly/platform/internal/mobilegateway"
)

// Store is the DynamoDB-backed mobilegateway.DeviceStore.
type Store struct {
	db    *dynamodb.Client
	table string
}

// New builds a Store over the given AWS config and table name. optFns lets the
// caller override the resolved endpoint (DYNAMODB_ENDPOINT → dynamodb-local for
// tests/local runs); in AWS the SDK resolves the real endpoint and Pod Identity
// supplies credentials — no static keys. Mirrors the registry dynamostore.New.
func New(cfg aws.Config, table string, optFns ...func(*dynamodb.Options)) *Store {
	return &Store{db: dynamodb.NewFromConfig(cfg, optFns...), table: table}
}

// Compile-time proof the DynamoDB store satisfies the neutral contract.
var _ mobilegateway.DeviceStore = (*Store)(nil)

func userPK(userID string) string { return "USER#" + userID }

func strAV(s string) types.AttributeValue { return &types.AttributeValueMemberS{Value: s} }

// marshal/unmarshal use the Device JSON tags as attribute names (id, platform,
// pushToken, createdAt), matching the registry store's convention so no parallel
// dynamodbav tag set is maintained. UserID is `json:"-"` so it is NOT stored as
// its own attribute — it lives only in the pk, and ListByUser restores it from
// the queried partition.
func marshal(v any) (map[string]types.AttributeValue, error) {
	return attributevalue.MarshalMapWithOptions(v, func(o *attributevalue.EncoderOptions) { o.TagKey = "json" })
}

func unmarshal(m map[string]types.AttributeValue, out any) error {
	return attributevalue.UnmarshalMapWithOptions(m, out, func(o *attributevalue.DecoderOptions) { o.TagKey = "json" })
}

// Put inserts or replaces a device, keyed by (UserID, ID). It validates BEFORE
// any I/O — exactly like the in-memory impl — so a half-formed record (empty
// userId ⇒ a bare "USER#" partition, or empty id ⇒ an empty sort key) can never
// be written.
func (s *Store) Put(ctx context.Context, d mobilegateway.Device) error {
	if d.ID == "" || d.UserID == "" || d.PushToken == "" {
		return mobilegateway.ErrInvalidDevice
	}
	item, err := marshal(d)
	if err != nil {
		return err
	}
	// The UserID lives only in the partition key (it is json:"-", so marshal
	// dropped it); the deviceId is both the sort key and the stored `id` attribute.
	item["pk"] = strAV(userPK(d.UserID))
	item["sk"] = strAV(d.ID)
	_, err = s.db.PutItem(ctx, &dynamodb.PutItemInput{TableName: &s.table, Item: item})
	return err
}

// Delete removes a device, but only within userID's own partition. ALL_OLD
// returns the deleted item's attributes, so a present device (found=true) is
// distinguished from a no-op delete (found=false) without a separate read — and
// because the key is (USER#<userID>, deviceID), a device owned by another user is
// simply not on this key path, so it reports not-found and is never touched.
func (s *Store) Delete(ctx context.Context, userID, deviceID string) (bool, error) {
	out, err := s.db.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName:    &s.table,
		Key:          map[string]types.AttributeValue{"pk": strAV(userPK(userID)), "sk": strAV(deviceID)},
		ReturnValues: types.ReturnValueAllOld,
	})
	if err != nil {
		return false, err
	}
	return len(out.Attributes) > 0, nil
}

// ListByUser returns every device in userID's partition (possibly empty). The
// query cannot cross into another user's partition, so the result is user-scoped
// by construction.
func (s *Store) ListByUser(ctx context.Context, userID string) ([]mobilegateway.Device, error) {
	var items []map[string]types.AttributeValue
	p := dynamodb.NewQueryPaginator(s.db, &dynamodb.QueryInput{
		TableName:                 &s.table,
		KeyConditionExpression:    aws.String("pk = :pk"),
		ExpressionAttributeValues: map[string]types.AttributeValue{":pk": strAV(userPK(userID))},
		// Strongly consistent — deliberately unlike Phase 1's registry list Queries,
		// which run on GSIs where ConsistentRead is forbidden; this Query is on the
		// base table, so it is legal. It preserves the read-your-writes contract the
		// in-memory store gives the handlers: without it, the notify fan-out could
		// list-and-miss a phone registered moments before a consent prompt and skip
		// its very first push. At 1–5 devices per user the doubled read cost is noise.
		ConsistentRead: aws.Bool(true),
	})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		items = append(items, page.Items...)
	}
	out := make([]mobilegateway.Device, 0, len(items))
	for _, m := range items {
		var d mobilegateway.Device
		if err := unmarshal(m, &d); err != nil {
			return nil, err
		}
		// UserID is json:"-", so it was never a stored attribute — restore it from
		// the partition we queried, matching the in-memory store's returned Device.
		d.UserID = userID
		out = append(out, d)
	}
	return out, nil
}
