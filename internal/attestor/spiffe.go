package attestor

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/spiffe/go-spiffe/v2/svid/jwtsvid"
	"github.com/spiffe/go-spiffe/v2/workloadapi"
)

// SpiffeSource fetches JWT-SVIDs from the SPIRE workload API over the
// workload-API socket mounted into the pod. It is the only file in this
// package that imports go-spiffe — the adapter, per the dependency-direction
// rule.
type SpiffeSource struct {
	// SocketPath is the SPIFFE workload-API endpoint (unix://...).
	SocketPath string
	// Retries and RetryDelay bound the startup wait while SPIRE delivers an
	// identity to the freshly created pod. Zero values fall back to the
	// historical defaults (10 attempts, 3s apart).
	Retries    int
	RetryDelay time.Duration
}

// Fetch retrieves a JWT-SVID for the given audience, retrying while SPIRE is
// still attesting the workload at startup.
func (s *SpiffeSource) Fetch(ctx context.Context, audience string) (Credential, error) {
	retries := s.Retries
	if retries <= 0 {
		retries = 10
	}
	delay := s.RetryDelay
	if delay <= 0 {
		delay = 3 * time.Second
	}

	var err error
	for i := 0; i < retries; i++ {
		var svid *jwtsvid.SVID
		svid, err = workloadapi.FetchJWTSVID(ctx,
			jwtsvid.Params{Audience: audience},
			workloadapi.WithAddr(s.SocketPath),
		)
		if err == nil {
			return Credential{Value: svid.Marshal(), AssertionType: JWTBearerAssertionType}, nil
		}
		log.Printf("waiting for SPIRE identity (attempt %d/%d): %v", i+1, retries, err)
		select {
		case <-ctx.Done():
			return Credential{}, ctx.Err()
		case <-time.After(delay):
		}
	}
	return Credential{}, fmt.Errorf("fetch JWT-SVID (audience=%s): %w", audience, err)
}
