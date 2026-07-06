// dynamostore integration tests, gated on DYNAMODB_TEST_ENDPOINT so the fast,
// secret-free unit lane skips them. `make test-dynamodb` (and the matching CI
// step) starts amazon/dynamodb-local, sets the endpoint + dummy AWS creds, and
// runs these — proving the DynamoDB Store passes the SAME storetest suite as the
// in-memory store (spec criterion 6, registry half) plus the concurrent
// create-consent-request invariant the single-table transaction guarantees.
package dynamostore

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/spawnly/platform/internal/registry"
	"github.com/spawnly/platform/internal/registry/storetest"
)

// newTestStore stands up a uniquely-named table (pk/sk + gsi1 + gsi2) against
// dynamodb-local, seeds the default schema (as main() does on boot), and
// registers cleanup to drop the table. It skips the test when the endpoint is
// unset so the default `go test ./...` lane stays green without a container.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	endpoint := os.Getenv("DYNAMODB_TEST_ENDPOINT")
	if endpoint == "" {
		t.Skip("DYNAMODB_TEST_ENDPOINT not set; skipping dynamodb-local integration test")
	}
	ctx := context.Background()

	// Dummy static credentials: dynamodb-local ignores them, but the SDK still
	// signs requests, so it needs some credentials + a region resolved.
	cfg, err := config.LoadDefaultConfig(ctx,
		config.WithRegion("us-east-1"),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider("dummy", "dummy", "")),
	)
	if err != nil {
		t.Fatalf("load AWS config: %v", err)
	}
	db := dynamodb.NewFromConfig(cfg, func(o *dynamodb.Options) { o.BaseEndpoint = aws.String(endpoint) })

	table := fmt.Sprintf("spawnly-registry-test-%d", time.Now().UnixNano())
	createTable(t, ctx, db, table)
	t.Cleanup(func() {
		if _, err := db.DeleteTable(context.Background(), &dynamodb.DeleteTableInput{TableName: &table}); err != nil {
			t.Logf("cleanup: delete table %s: %v", table, err)
		}
	})

	s := New(cfg, table, func(o *dynamodb.Options) { o.BaseEndpoint = aws.String(endpoint) })
	// The suite's ConsentExpiry/ValidateTemplate paths and the create handler
	// expect a parsed schema; main() applies it on boot, so mirror that here.
	s.SetSchema(defaultTestSchema, "test", "test")
	return s
}

// defaultTestSchema mirrors the tenant/agent shape the platform default schema
// declares — enough for the templates the suite creates to validate cleanly.
const defaultTestSchema = `definition tenant {
	relation agent: agent
	permission work_on = agent
}
definition agent {
	relation enabled: agent
}`

func createTable(t *testing.T, ctx context.Context, db *dynamodb.Client, table string) {
	t.Helper()
	s := types.ScalarAttributeTypeS
	_, err := db.CreateTable(ctx, &dynamodb.CreateTableInput{
		TableName:   &table,
		BillingMode: types.BillingModePayPerRequest,
		AttributeDefinitions: []types.AttributeDefinition{
			{AttributeName: aws.String("pk"), AttributeType: s},
			{AttributeName: aws.String("sk"), AttributeType: s},
			{AttributeName: aws.String("gsi1pk"), AttributeType: s},
			{AttributeName: aws.String("gsi1sk"), AttributeType: s},
			{AttributeName: aws.String("gsi2pk"), AttributeType: s},
		},
		KeySchema: []types.KeySchemaElement{
			{AttributeName: aws.String("pk"), KeyType: types.KeyTypeHash},
			{AttributeName: aws.String("sk"), KeyType: types.KeyTypeRange},
		},
		GlobalSecondaryIndexes: []types.GlobalSecondaryIndex{
			{
				IndexName: aws.String("gsi1"),
				KeySchema: []types.KeySchemaElement{
					{AttributeName: aws.String("gsi1pk"), KeyType: types.KeyTypeHash},
					{AttributeName: aws.String("gsi1sk"), KeyType: types.KeyTypeRange},
				},
				Projection: &types.Projection{ProjectionType: types.ProjectionTypeAll},
			},
			{
				// gsi2 is a hash-only index (PARENT#/CONID#/EDGE# lookups need no sort key).
				IndexName: aws.String("gsi2"),
				KeySchema: []types.KeySchemaElement{
					{AttributeName: aws.String("gsi2pk"), KeyType: types.KeyTypeHash},
				},
				Projection: &types.Projection{ProjectionType: types.ProjectionTypeAll},
			},
		},
	})
	if err != nil {
		t.Fatalf("create table %s: %v", table, err)
	}
	if err := dynamodb.NewTableExistsWaiter(db).Wait(ctx,
		&dynamodb.DescribeTableInput{TableName: &table}, 30*time.Second); err != nil {
		t.Fatalf("wait for table %s active: %v", table, err)
	}
}

// TestStoreSuite_DynamoDB runs the shared conformance suite against dynamodb-local.
func TestStoreSuite_DynamoDB(t *testing.T) {
	s := newTestStore(t)
	storetest.Run(t, func(t *testing.T) registry.Store { return s })
}

// TestConcurrentCreateConsentRequest asserts the single-table TransactWriteItems
// makes create-consent-request race-safe: N goroutines racing to open the same
// edge produce EXACTLY ONE created=true, and every caller sees the same request
// id (the winner's). This is the durable equivalent of the in-memory single-lock
// openRequests dedup.
func TestConcurrentCreateConsentRequest(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	const n = 12
	req := registry.ConsentRequest{
		UserID: "conc-user", ParentType: "conc-parent", ChildType: "conc-child",
		AgentID: "conc-agent", Scopes: []string{"x"},
	}

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		created int
		ids     = map[string]struct{}{}
		errs    []error
	)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cr, wasCreated, err := s.CreateConsentRequest(ctx, req)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			if wasCreated {
				created++
			}
			ids[cr.ID] = struct{}{}
		}()
	}
	wg.Wait()

	for _, err := range errs {
		t.Errorf("concurrent create errored: %v", err)
	}
	if created != 1 {
		t.Errorf("created=true count = %d, want exactly 1", created)
	}
	if len(ids) != 1 {
		t.Errorf("distinct request ids = %d (%v), want 1 (all callers see the winner)", len(ids), ids)
	}
}

// TestResolveDoesNotClobberNewerMarker exercises the marker-clobber race
// (finding 2), which is invisible to the concurrency test above because it only
// manifests when a resolve is delayed between its status pivot and its marker
// cleanup while a fresh request installs a NEW marker for the same key. The
// afterPivot seam injects exactly that interleave: while resolving R1 (post-pivot,
// R1's marker still present), a concurrent create observes R1 as no-longer-pending,
// cleans the stale marker, and installs R2 + a new marker. The resolve must then
// NOT delete R2's marker — so R2 stays the single open request for the key.
func TestResolveDoesNotClobberNewerMarker(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// R1 requests scope [x]; the interleaved R2 requests [x,z]. Both share the
	// agent (hence the marker key), but R2's extra scope keeps it OUT of R1's
	// approve-sweep — so if R2 ends up resolved it can only be because its marker
	// was clobbered and something went wrong, not because the sweep covered it.
	newReq := func(scopes ...string) registry.ConsentRequest {
		return registry.ConsentRequest{
			UserID: "clob-user", ParentType: "clob-parent", ChildType: "clob-child",
			AgentID: "clob-agent", Scopes: scopes,
		}
	}

	r1, created, err := s.CreateConsentRequest(ctx, newReq("x"))
	if err != nil || !created {
		t.Fatalf("create R1: created=%v err=%v", created, err)
	}

	// During R1's resolve (after the pivot, before its marker cleanup) a fresh
	// create races in: R1 is now approved, so this cleans R1's stale marker and
	// installs R2 with its own marker.
	var r2 registry.ConsentRequest
	s.afterPivot = func() {
		s.afterPivot = nil // run once
		var cerr error
		var wasCreated bool
		r2, wasCreated, cerr = s.CreateConsentRequest(ctx, newReq("x", "z"))
		if cerr != nil || !wasCreated {
			t.Errorf("interleaved create R2: created=%v err=%v", wasCreated, cerr)
		}
	}

	// Approve R1 with only [x], so the resulting grant does not cover R2's [x,z].
	if _, ok, err := s.ResolveConsentRequest(ctx, r1.ID, "", true, []string{"x"}); err != nil || !ok {
		t.Fatalf("resolve R1: ok=%v err=%v", ok, err)
	}
	if r2.ID == "" || r2.ID == r1.ID {
		t.Fatalf("interleaved create did not produce a distinct R2 (r1=%q r2=%q)", r1.ID, r2.ID)
	}

	// R2 must still be pending, and its marker must be intact: a follow-up create
	// for the same key dedupes to R2 (created=false, same id). Under the old
	// unconditional marker delete, R1's resolve would have removed R2's marker and
	// this create would mint a THIRD request — two live pending requests for one key.
	if got, ok, _ := s.GetConsentRequest(ctx, r2.ID); !ok || got.Status != registry.ConsentPending {
		t.Fatalf("R2 = %+v ok=%v, want still pending", got, ok)
	}
	r3, created3, err := s.CreateConsentRequest(ctx, newReq())
	if err != nil {
		t.Fatalf("create R3: %v", err)
	}
	if created3 {
		t.Errorf("follow-up create reported created=true — R2's marker was clobbered")
	}
	if r3.ID != r2.ID {
		t.Errorf("follow-up create id = %q, want dedupe to R2 %q", r3.ID, r2.ID)
	}
}
