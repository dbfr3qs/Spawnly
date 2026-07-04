// External test package (mobilegateway_test) so it can import the shared
// storetest suite without an import cycle: storetest imports mobilegateway, so a
// runner inside package mobilegateway would cycle. The DynamoDB runner in
// package dynamostore runs the SAME suite against dynamodb-local — see
// internal/mobilegateway/dynamostore/store_test.go.
package mobilegateway_test

import (
	"testing"

	"github.com/spawnly/platform/internal/mobilegateway"
	"github.com/spawnly/platform/internal/mobilegateway/storetest"
)

// TestDeviceStoreSuite_InMemory holds the default in-memory MemoryDeviceStore to
// the same behavioural contract as the durable DynamoDB store.
func TestDeviceStoreSuite_InMemory(t *testing.T) {
	storetest.RunDeviceStoreSuite(t, func(t *testing.T) mobilegateway.DeviceStore {
		return mobilegateway.NewMemoryDeviceStore()
	})
}
