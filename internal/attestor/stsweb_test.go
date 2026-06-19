package attestor

import (
	"context"
	"errors"
	"testing"
)

func TestStsWebSource_Fetch(t *testing.T) {
	s := &StsWebSource{
		audience: "spawnly",
		fetch:    func(context.Context) (string, error) { return "header.payload.sig", nil },
	}
	cred, err := s.Fetch(context.Background(), "ignored-audience")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if cred.Value != "header.payload.sig" {
		t.Fatalf("Value = %q", cred.Value)
	}
	if cred.AssertionType != JWTBearerAssertionType {
		t.Fatalf("AssertionType = %q, want %q", cred.AssertionType, JWTBearerAssertionType)
	}
}

func TestStsWebSource_FetchError(t *testing.T) {
	sentinel := errors.New("sts boom")
	s := &StsWebSource{fetch: func(context.Context) (string, error) { return "", sentinel }}
	if _, err := s.Fetch(context.Background(), ""); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want %v", err, sentinel)
	}
}
