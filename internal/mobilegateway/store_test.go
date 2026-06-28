package mobilegateway

import (
	"context"
	"testing"
	"time"
)

func dev(id, user, token string) Device {
	return Device{ID: id, UserID: user, Platform: PlatformIOS, PushToken: token, CreatedAt: time.Now()}
}

func TestMemoryDeviceStore_PutValidation(t *testing.T) {
	s := NewMemoryDeviceStore()
	for _, d := range []Device{
		{UserID: "u", PushToken: "t"}, // no id
		{ID: "d", PushToken: "t"},     // no user
		{ID: "d", UserID: "u"},        // no token
	} {
		if err := s.Put(context.Background(), d); err != ErrInvalidDevice {
			t.Fatalf("Put(%+v) = %v, want ErrInvalidDevice", d, err)
		}
	}
}

func TestMemoryDeviceStore_PerUserIsolation(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryDeviceStore()
	if err := s.Put(ctx, dev("d1", "alice", "ta")); err != nil {
		t.Fatal(err)
	}
	if err := s.Put(ctx, dev("d2", "bob", "tb")); err != nil {
		t.Fatal(err)
	}

	alice, _ := s.ListByUser(ctx, "alice")
	if len(alice) != 1 || alice[0].ID != "d1" {
		t.Fatalf("alice sees %+v, want only d1", alice)
	}

	// Bob cannot delete Alice's device even by guessing its id.
	ok, _ := s.Delete(ctx, "bob", "d1")
	if ok {
		t.Fatal("bob deleted alice's device")
	}
	alice, _ = s.ListByUser(ctx, "alice")
	if len(alice) != 1 {
		t.Fatalf("alice's device gone after bob's delete: %+v", alice)
	}

	// Alice can delete her own.
	ok, _ = s.Delete(ctx, "alice", "d1")
	if !ok {
		t.Fatal("alice could not delete her own device")
	}
	alice, _ = s.ListByUser(ctx, "alice")
	if len(alice) != 0 {
		t.Fatalf("alice still has devices: %+v", alice)
	}
}

func TestMemoryDeviceStore_PutReplaces(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryDeviceStore()
	s.Put(ctx, dev("d1", "alice", "old"))
	s.Put(ctx, dev("d1", "alice", "new"))
	got, _ := s.ListByUser(ctx, "alice")
	if len(got) != 1 || got[0].PushToken != "new" {
		t.Fatalf("got %+v, want one device with token 'new'", got)
	}
}
