package attestor

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestMockSource(t *testing.T) {
	want := Credential{Value: "tok", AssertionType: JWTBearerAssertionType}
	m := &MockSource{Cred: want}
	got, err := m.Fetch(context.Background(), "registry")
	if err != nil || got != want {
		t.Fatalf("Fetch = %+v, %v; want %+v, nil", got, err, want)
	}

	sentinel := errors.New("boom")
	if _, err := (&MockSource{Err: sentinel}).Fetch(context.Background(), "x"); !errors.Is(err, sentinel) {
		t.Fatalf("Fetch err = %v; want %v", err, sentinel)
	}
}

// SpiffeSource must surface a cancelled context promptly instead of blocking
// the whole retry budget when SPIRE never answers.
func TestSpiffeSource_ContextCancelled(t *testing.T) {
	s := &SpiffeSource{
		SocketPath: "unix:///nonexistent/spire-agent.sock",
		Retries:    5,
		RetryDelay: time.Hour, // long: the test must finish via ctx, not the delay
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan error, 1)
	go func() { _, err := s.Fetch(ctx, "registry"); done <- err }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected error from cancelled context")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Fetch did not honor context cancellation")
	}
}
