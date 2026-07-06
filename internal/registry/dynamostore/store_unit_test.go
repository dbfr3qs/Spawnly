// Pure-logic unit tests that need no dynamodb-local, so they run in the default
// `go test ./...` lane (unlike store_test.go, which is gated on
// DYNAMODB_TEST_ENDPOINT). They cover the TransactionConflict retry helper the
// approve path reuses — a path dynamodb-local can't exercise because it never
// emits a TransactionConflict cancellation.
package dynamostore

import (
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

func conflictErr() error {
	return &types.TransactionCanceledException{
		CancellationReasons: []types.CancellationReason{{Code: aws.String("TransactionConflict")}},
	}
}

func TestRetryOnTxnConflict(t *testing.T) {
	t.Run("success first try", func(t *testing.T) {
		calls := 0
		err := retryOnTxnConflict(func() error { calls++; return nil })
		if err != nil || calls != 1 {
			t.Fatalf("got err=%v calls=%d, want nil/1", err, calls)
		}
	})

	t.Run("retries conflict then succeeds", func(t *testing.T) {
		calls := 0
		err := retryOnTxnConflict(func() error {
			calls++
			if calls < 3 {
				return conflictErr()
			}
			return nil
		})
		if err != nil || calls != 3 {
			t.Fatalf("got err=%v calls=%d, want nil/3", err, calls)
		}
	})

	t.Run("persistent conflict exhausts budget and surfaces the error", func(t *testing.T) {
		calls := 0
		err := retryOnTxnConflict(func() error { calls++; return conflictErr() })
		if !isTxnConflict(err) {
			t.Fatalf("got err=%v, want a surfaced TransactionConflict", err)
		}
		if calls != txnConflictBudget {
			t.Fatalf("attempts = %d, want %d", calls, txnConflictBudget)
		}
	})

	t.Run("non-conflict error returns immediately without retry", func(t *testing.T) {
		calls := 0
		sentinel := errors.New("boom")
		err := retryOnTxnConflict(func() error { calls++; return sentinel })
		if !errors.Is(err, sentinel) || calls != 1 {
			t.Fatalf("got err=%v calls=%d, want boom/1", err, calls)
		}
	})
}
