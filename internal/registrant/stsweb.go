package registrant

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/lestrrat-go/jwx/v2/jwk"
	"github.com/lestrrat-go/jwx/v2/jws"
	"github.com/lestrrat-go/jwx/v2/jwt"
)

// stsNamespaceClaim is the namespaced claim STS puts identity context under in a
// GetWebIdentityToken JWT.
const stsNamespaceClaim = "https://sts.amazonaws.com/"

// StsWebConfig configures an StsWebVerifier.
type StsWebConfig struct {
	// JWKSURL is the account STS issuer's JWKS endpoint
	// (https://<id>.tokens.sts.global.api.aws/.well-known/jwks.json). Required.
	JWKSURL string
	// Issuer, if set, is enforced as the expected `iss`.
	Issuer string
	// Audience is the expected `aud` (the control-plane audience).
	Audience string
	// Namespace / ServiceAccount / ClusterARN, if set, are asserted against the
	// EKS-attested principal_tags for defense in depth.
	Namespace      string
	ServiceAccount string
	ClusterARN     string
	// PodSuffix is stripped from kubernetes-pod-name to get the agentId
	// (operator names pods "<agentId>-pod"). Defaults to "-pod".
	PodSuffix string
}

// StsWebVerifier authenticates a registration request whose bearer token is an
// AWS STS web-identity token (sts:GetWebIdentityToken) issued to a pod running
// under EKS Pod Identity. The agentId comes from the EKS-set, cluster-attested
// principal_tags.kubernetes-pod-name — NOT from caller-supplied request_tags,
// which a workload could forge. The adapter that bridges the AWS-signed JWT to
// the neutral Identity.
type StsWebVerifier struct {
	cfg   StsWebConfig
	cache *jwk.Cache
}

// NewStsWebVerifier primes a JWKS cache for cfg.JWKSURL (the STS issuer's JWKS
// is public, TLS-valid — no insecure skip).
func NewStsWebVerifier(ctx context.Context, cfg StsWebConfig) (*StsWebVerifier, error) {
	if cfg.JWKSURL == "" {
		return nil, errors.New("StsWebConfig.JWKSURL is required")
	}
	if cfg.PodSuffix == "" {
		cfg.PodSuffix = "-pod"
	}
	cache := jwk.NewCache(ctx)
	if err := cache.Register(cfg.JWKSURL); err != nil {
		return nil, err
	}
	if _, err := cache.Get(ctx, cfg.JWKSURL); err != nil {
		return nil, fmt.Errorf("fetch STS JWKS: %w", err)
	}
	return &StsWebVerifier{cfg: cfg, cache: cache}, nil
}

// Verify validates the bearer JWT and derives Identity from the attested
// principal_tags.
func (v *StsWebVerifier) Verify(ctx context.Context, r *http.Request) (Identity, error) {
	raw := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if raw == "" {
		return Identity{}, errors.New("missing web-identity token")
	}
	keySet, err := v.cache.Get(ctx, v.cfg.JWKSURL)
	if err != nil {
		return Identity{}, fmt.Errorf("get JWKS: %w", err)
	}
	// The AWS STS JWKS RSA key omits the `alg` field; tell jwx to infer the
	// algorithm from the key (matched by kid) instead of refusing to verify.
	opts := []jwt.ParseOption{
		jwt.WithKeySet(keySet, jws.WithInferAlgorithmFromKey(true)),
		jwt.WithValidate(true),
	}
	if v.cfg.Audience != "" {
		opts = append(opts, jwt.WithAudience(v.cfg.Audience))
	}
	if v.cfg.Issuer != "" {
		opts = append(opts, jwt.WithIssuer(v.cfg.Issuer))
	}
	tok, err := jwt.Parse([]byte(raw), opts...)
	if err != nil {
		return Identity{}, fmt.Errorf("invalid web-identity token: %w", err)
	}

	claim, _ := tok.Get(stsNamespaceClaim)
	tags, err := principalTags(claim)
	if err != nil {
		return Identity{}, err
	}
	return v.cfg.identityFromTags(tags)
}

// principalTags extracts the attested principal_tags map from the STS namespaced
// claim value (as decoded by jwx into nested maps).
func principalTags(claim any) (map[string]any, error) {
	ns, ok := claim.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("token missing %q object", stsNamespaceClaim)
	}
	pt, ok := ns["principal_tags"].(map[string]any)
	if !ok {
		return nil, errors.New("token missing principal_tags (is the pod under EKS Pod Identity?)")
	}
	return pt, nil
}

// identityFromTags derives the neutral Identity from the attested principal_tags,
// asserting any configured expected values. Split out for testing without JWT
// signing.
func (cfg StsWebConfig) identityFromTags(tags map[string]any) (Identity, error) {
	tagStr := func(k string) string { s, _ := tags[k].(string); return s }

	for _, c := range []struct{ key, want string }{
		{"kubernetes-namespace", cfg.Namespace},
		{"kubernetes-service-account", cfg.ServiceAccount},
		{"eks-cluster-arn", cfg.ClusterARN},
	} {
		if c.want != "" && tagStr(c.key) != c.want {
			return Identity{}, fmt.Errorf("principal_tags.%s=%q, expected %q", c.key, tagStr(c.key), c.want)
		}
	}

	podName := tagStr("kubernetes-pod-name")
	if podName == "" {
		return Identity{}, errors.New("token missing principal_tags.kubernetes-pod-name")
	}
	suffix := cfg.PodSuffix
	if suffix == "" {
		suffix = "-pod"
	}
	agentID := strings.TrimSuffix(podName, suffix)
	if agentID == "" {
		return Identity{}, fmt.Errorf("pod name %q yields empty agentId", podName)
	}

	clusterArn := tagStr("eks-cluster-arn")
	if clusterArn == "" {
		clusterArn = "eks"
	}
	// Subject is path-style so downstream act-chain handling (path.Base) recovers
	// the agentId, matching the SPIFFE verifier's behavior.
	subject := fmt.Sprintf("%s/agent/%s", clusterArn, agentID)
	return Identity{AgentID: agentID, Subject: subject, Issuer: "aws-stsweb"}, nil
}
