// dynamostore integration test, gated on DYNAMODB_TEST_ENDPOINT so the fast,
// secret-free unit lane skips it. `make test-dynamodb` (and the matching CI step)
// starts amazon/dynamodb-local, sets the endpoint + dummy AWS creds, and runs
// this — proving the DynamoDB DeviceStore passes the SAME storetest suite as the
// in-memory store (spec criterion 6, devices half). Its package glob
// (./internal/mobilegateway/...) already covers this test, so no Makefile/CI
// change is needed.
package dynamostore

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/spawnly/platform/internal/mobilegateway"
	"github.com/spawnly/platform/internal/mobilegateway/storetest"
)

// newTestStore stands up a uniquely-named table (pk/sk only — the devices table
// needs no GSI) against dynamodb-local and registers cleanup to drop it. It skips
// the test when the endpoint is unset so the default `go test ./...` lane stays
// green without a container.
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

	table := fmt.Sprintf("spawnly-devices-test-%d", time.Now().UnixNano())
	createTable(t, ctx, db, table)

	return New(cfg, table, func(o *dynamodb.Options) { o.BaseEndpoint = aws.String(endpoint) })
}

func createTable(t *testing.T, ctx context.Context, db *dynamodb.Client, table string) {
	t.Helper()
	s := types.ScalarAttributeTypeS
	_, err := db.CreateTable(ctx, &dynamodb.CreateTableInput{
		TableName:   &table,
		BillingMode: types.BillingModePayPerRequest,
		AttributeDefinitions: []types.AttributeDefinition{
			{AttributeName: aws.String("pk"), AttributeType: s},
			{AttributeName: aws.String("sk"), AttributeType: s},
		},
		KeySchema: []types.KeySchemaElement{
			{AttributeName: aws.String("pk"), KeyType: types.KeyTypeHash},
			{AttributeName: aws.String("sk"), KeyType: types.KeyTypeRange},
		},
	})
	if err != nil {
		t.Fatalf("create table %s: %v", table, err)
	}
	// Register the drop as soon as the table exists — BEFORE the waiter, whose
	// t.Fatalf would otherwise skip a later registration and leak the table into
	// the shared dynamodb-local container.
	t.Cleanup(func() {
		if _, err := db.DeleteTable(context.Background(), &dynamodb.DeleteTableInput{TableName: &table}); err != nil {
			t.Logf("cleanup: delete table %s: %v", table, err)
		}
	})
	if err := dynamodb.NewTableExistsWaiter(db).Wait(ctx,
		&dynamodb.DescribeTableInput{TableName: &table}, 30*time.Second); err != nil {
		t.Fatalf("wait for table %s active: %v", table, err)
	}
}

// TestDeviceStoreSuite_DynamoDB runs the shared conformance suite against dynamodb-local.
func TestDeviceStoreSuite_DynamoDB(t *testing.T) {
	s := newTestStore(t)
	storetest.RunDeviceStoreSuite(t, func(t *testing.T) mobilegateway.DeviceStore { return s })
}
