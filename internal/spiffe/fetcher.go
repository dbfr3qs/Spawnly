// internal/spiffe/fetcher.go
package spiffe

import (
	"context"
	"fmt"

	"github.com/spiffe/go-spiffe/v2/svid/jwtsvid"
	"github.com/spiffe/go-spiffe/v2/workloadapi"
)

type SVIDFetcher interface {
	FetchJWT(ctx context.Context, audience string) (string, error)
}

type WorkloadFetcher struct{ SocketPath string }

func (f *WorkloadFetcher) FetchJWT(ctx context.Context, audience string) (string, error) {
	svid, err := workloadapi.FetchJWTSVID(ctx,
		jwtsvid.Params{Audience: audience},
		workloadapi.WithAddr(f.SocketPath),
	)
	if err != nil {
		return "", fmt.Errorf("fetch JWT-SVID (audience=%s): %w", audience, err)
	}
	return svid.Marshal(), nil
}

type MockFetcher struct {
	Tokens map[string]string // audience → token
	Err    error
}

func (m *MockFetcher) FetchJWT(_ context.Context, audience string) (string, error) {
	if m.Err != nil {
		return "", m.Err
	}
	return m.Tokens[audience], nil
}
