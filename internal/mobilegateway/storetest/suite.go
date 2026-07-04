// Package storetest is the shared behavioural conformance suite for the
// mobilegateway.DeviceStore contract. Every DeviceStore implementation (the
// in-memory MemoryDeviceStore, the DynamoDB store in package dynamostore) runs
// the exact same suite, so both are held to one definition of the contract — the
// persistence seam can be swapped (memory on kind, DynamoDB on AWS) without
// silently drifting the device-registry semantics the gateway handlers depend on.
//
// The suite encodes the ACTUAL in-memory semantics: ErrInvalidDevice on a
// missing id/userId/pushToken BEFORE any I/O, put-replaces-by-(user,id),
// delete-your-own reports true, a cross-user delete reports false and never
// touches the other user's device (per-user isolation is structural — the key
// includes the user), and ListByUser returns exactly that user's devices.
//
// # Isolation
//
// RunDeviceStoreSuite calls newStore ONCE and shares that store across all
// sub-tests (matching the DynamoDB runner, which provisions a single
// dynamodb-local table per run). Sub-tests therefore use UNIQUE user ids so they
// never collide, and never assert global totals — only per-user lists.
package storetest

import (
	"context"
	"testing"
	"time"

	"github.com/spawnly/platform/internal/mobilegateway"
)

// RunDeviceStoreSuite executes the full conformance suite against the store
// returned by newStore.
func RunDeviceStoreSuite(t *testing.T, newStore func(t *testing.T) mobilegateway.DeviceStore) {
	s := newStore(t)
	ctx := context.Background()

	t.Run("PutValidation", func(t *testing.T) { testPutValidation(t, ctx, s) })
	t.Run("PutReplaces", func(t *testing.T) { testPutReplaces(t, ctx, s) })
	t.Run("PerUserIsolation", func(t *testing.T) { testPerUserIsolation(t, ctx, s) })
	t.Run("ListEmptyAndMultiple", func(t *testing.T) { testListEmptyAndMultiple(t, ctx, s) })
}

// dev builds a valid device for the given ids. Platform is a valid value so only
// the field under test is ever the reason a Put is rejected.
func dev(id, user, token string) mobilegateway.Device {
	return mobilegateway.Device{
		ID:        id,
		UserID:    user,
		Platform:  mobilegateway.PlatformIOS,
		PushToken: token,
		CreatedAt: time.Now().UTC(),
	}
}

func testPutValidation(t *testing.T, ctx context.Context, s mobilegateway.DeviceStore) {
	// Missing id / userId / pushToken each yields ErrInvalidDevice, and the
	// validation must happen BEFORE any I/O (the DynamoDB impl checks up front,
	// same as the in-memory one) — otherwise a half-formed key could be written.
	for _, d := range []mobilegateway.Device{
		{UserID: "st-val-u", PushToken: "t"}, // no id
		{ID: "st-val-d", PushToken: "t"},     // no user
		{ID: "st-val-d", UserID: "st-val-u"}, // no token
	} {
		if err := s.Put(ctx, d); err != mobilegateway.ErrInvalidDevice {
			t.Errorf("Put(%+v) = %v, want ErrInvalidDevice", d, err)
		}
	}
	// A rejected Put must not have created anything the user can list.
	if got, err := s.ListByUser(ctx, "st-val-u"); err != nil || len(got) != 0 {
		t.Errorf("ListByUser after rejected Puts = %+v err=%v, want empty", got, err)
	}
}

func testPutReplaces(t *testing.T, ctx context.Context, s mobilegateway.DeviceStore) {
	const user = "st-replace-user"
	if err := s.Put(ctx, dev("d1", user, "old")); err != nil {
		t.Fatalf("Put(old): %v", err)
	}
	// Same (user, id) replaces in place — a phone re-registering with a rotated
	// push token overwrites, it does not accumulate a second row.
	if err := s.Put(ctx, dev("d1", user, "new")); err != nil {
		t.Fatalf("Put(new): %v", err)
	}
	got, err := s.ListByUser(ctx, user)
	if err != nil {
		t.Fatalf("ListByUser: %v", err)
	}
	if len(got) != 1 || got[0].PushToken != "new" {
		t.Fatalf("ListByUser = %+v, want one device with token 'new'", got)
	}
	if got[0].Platform != mobilegateway.PlatformIOS {
		t.Errorf("replaced device platform = %q, want the round-tripped %q", got[0].Platform, mobilegateway.PlatformIOS)
	}
}

func testPerUserIsolation(t *testing.T, ctx context.Context, s mobilegateway.DeviceStore) {
	const (
		alice = "st-iso-alice"
		bob   = "st-iso-bob"
	)
	if err := s.Put(ctx, dev("d1", alice, "ta")); err != nil {
		t.Fatalf("Put(alice): %v", err)
	}
	if err := s.Put(ctx, dev("d2", bob, "tb")); err != nil {
		t.Fatalf("Put(bob): %v", err)
	}

	// Alice sees only her own device — the store key includes the user, so a list
	// can never cross the tenant boundary.
	al, err := s.ListByUser(ctx, alice)
	if err != nil {
		t.Fatalf("ListByUser(alice): %v", err)
	}
	if len(al) != 1 || al[0].ID != "d1" {
		t.Fatalf("alice sees %+v, want only d1", al)
	}

	// Bob cannot delete Alice's device even by guessing its id: the delete is
	// keyed by (bob, d1) which does not exist, so it reports false and leaves d1.
	if ok, err := s.Delete(ctx, bob, "d1"); err != nil || ok {
		t.Errorf("Delete(bob, alice's d1) = %v err=%v, want false", ok, err)
	}
	if al, _ := s.ListByUser(ctx, alice); len(al) != 1 {
		t.Fatalf("alice's device gone after bob's cross-user delete: %+v", al)
	}

	// Alice can delete her own.
	if ok, err := s.Delete(ctx, alice, "d1"); err != nil || !ok {
		t.Fatalf("Delete(alice, d1) = %v err=%v, want true", ok, err)
	}
	if al, _ := s.ListByUser(ctx, alice); len(al) != 0 {
		t.Fatalf("alice still has devices after deleting her own: %+v", al)
	}
	// Deleting a device that does not exist reports false, not an error.
	if ok, err := s.Delete(ctx, alice, "d1"); err != nil || ok {
		t.Errorf("Delete(gone) = %v err=%v, want false", ok, err)
	}
	// Bob's device is untouched throughout.
	if b, _ := s.ListByUser(ctx, bob); len(b) != 1 || b[0].ID != "d2" {
		t.Errorf("bob's devices = %+v, want only d2", b)
	}
}

func testListEmptyAndMultiple(t *testing.T, ctx context.Context, s mobilegateway.DeviceStore) {
	const user = "st-list-user"
	// A user with no registrations lists empty (never nil-panics on the caller).
	if got, err := s.ListByUser(ctx, user); err != nil || len(got) != 0 {
		t.Fatalf("ListByUser(empty) = %+v err=%v, want empty", got, err)
	}
	// Multiple devices for one user all come back.
	for _, id := range []string{"d1", "d2", "d3"} {
		if err := s.Put(ctx, dev(id, user, "tok-"+id)); err != nil {
			t.Fatalf("Put(%s): %v", id, err)
		}
	}
	got, err := s.ListByUser(ctx, user)
	if err != nil {
		t.Fatalf("ListByUser(multiple): %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("ListByUser(multiple) len = %d, want 3 (%+v)", len(got), ids(got))
	}
	for _, want := range []string{"d1", "d2", "d3"} {
		if !hasDevice(got, want) {
			t.Errorf("ListByUser missing device %q: %v", want, ids(got))
		}
	}
}

func hasDevice(ds []mobilegateway.Device, id string) bool {
	for _, d := range ds {
		if d.ID == id {
			return true
		}
	}
	return false
}

func ids(ds []mobilegateway.Device) []string {
	out := make([]string, len(ds))
	for i, d := range ds {
		out[i] = d.ID
	}
	return out
}
