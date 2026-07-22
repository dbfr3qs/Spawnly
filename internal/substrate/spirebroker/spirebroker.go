// Package spirebroker is the PRODUCTION-EDGE entry broker for the CP-mediated
// SPIRE Entry API: it drives SPIRE registration entries (and the rotate/disconnect
// node hygiene) by calling the SPIRE server's ENTRY + AGENT admin gRPC APIs as a
// delegated admin, over mTLS authenticated by the orchestrator's OWN SVID.
//
// # Why this exists / the execEntryBroker relationship
//
// The local-sim broker (cmd/orchestrator execEntryBroker) shells out to the
// `spire-server` CLI over a hostPath admin socket — fine for kind, but it MUST NOT
// ship to the real edge (no hostPath, no CLI). On AWS the orchestrator holds a
// SPIRE-issued SVID (its SPIFFE id is pinned in the server's admin_ids) and talks
// the entry/agent gRPC admin API directly. Both brokers sit behind the SAME narrow
// substrate.EntryBroker seam (+ ParentPrefixDeleter / NodeEvicter), so swapping the
// mechanism NEVER touches the authorization in substrate.EntryService — every spec
// that reaches this broker has already been validated org-correct. No authz lives
// here; the broker only translates canonical specs into gRPC calls.
//
// # Dependency direction
//
// This is an ADAPTER: it imports the SPIRE API SDK + go-spiffe and satisfies the
// SPIRE-SDK-blind substrate.EntryBroker seam. It is wired only in cmd/orchestrator.
// The neutral contract (internal/substrate) never imports this package or the SDK.
package spirebroker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/spiffetls/tlsconfig"
	"github.com/spiffe/go-spiffe/v2/workloadapi"
	agentv1 "github.com/spiffe/spire-api-sdk/proto/spire/api/server/agent/v1"
	entryv1 "github.com/spiffe/spire-api-sdk/proto/spire/api/server/entry/v1"
	"github.com/spiffe/spire-api-sdk/proto/spire/api/types"
	"google.golang.org/grpc/credentials"

	"github.com/spawnly/platform/internal/substrate"
)

// EntryAPI is the NARROW slice of the SPIRE server entry admin API this broker
// uses — exactly the three RPCs the entry lifecycle needs. Keeping it an interface
// lets unit tests dial a fake SPIRE entry server over bufconn (the real generated
// entryv1.EntryClient satisfies it), and documents that the broker can do nothing
// else with the delegated-admin credential (no bundle ops, no agent creation, etc.).
type EntryAPI interface {
	BatchCreateEntry(ctx context.Context, in *entryv1.BatchCreateEntryRequest, opts ...grpc.CallOption) (*entryv1.BatchCreateEntryResponse, error)
	ListEntries(ctx context.Context, in *entryv1.ListEntriesRequest, opts ...grpc.CallOption) (*entryv1.ListEntriesResponse, error)
	BatchDeleteEntry(ctx context.Context, in *entryv1.BatchDeleteEntryRequest, opts ...grpc.CallOption) (*entryv1.BatchDeleteEntryResponse, error)
}

// AgentAPI is the NARROW slice of the SPIRE server agent admin API this broker
// uses — only the two RPCs the node-eviction hygiene needs. The real generated
// agentv1.AgentClient satisfies it; tests substitute a bufconn fake.
type AgentAPI interface {
	ListAgents(ctx context.Context, in *agentv1.ListAgentsRequest, opts ...grpc.CallOption) (*agentv1.ListAgentsResponse, error)
	DeleteAgent(ctx context.Context, in *agentv1.DeleteAgentRequest, opts ...grpc.CallOption) (*emptypb.Empty, error)
}

// Broker is the gRPC substrate.EntryBroker. It implements the full capability set
// (create/delete + parent-prefix delete + node evict) so rotate/disconnect hygiene
// keeps working across the seam swap.
type Broker struct {
	entries EntryAPI
	agents  AgentAPI
	// closer releases the mTLS connection and the X509Source; nil for a Broker
	// constructed directly from injected clients (tests).
	closer io.Closer
}

// Compile-time proof the gRPC broker satisfies the SAME seams the exec broker does,
// so it is a drop-in behind substrate.EntryBroker with no authz change.
var (
	_ substrate.EntryBroker         = (*Broker)(nil)
	_ substrate.ParentPrefixDeleter = (*Broker)(nil)
	_ substrate.NodeEvicter         = (*Broker)(nil)
)

// Config parametrizes the mTLS gRPC broker.
type Config struct {
	// Addr is the SPIRE server entry/agent admin gRPC endpoint (host:port). It is
	// reached in-cluster (ClusterIP) — NOT the internet-facing NLB, which fronts
	// only node attestation.
	Addr string
	// WorkloadAPISocket is the SPIFFE Workload API endpoint the orchestrator reads
	// its OWN SVID from (e.g. "unix:///spiffe-workload-api/spire-agent.sock"). Empty
	// ⇒ go-spiffe uses the standard SPIFFE_ENDPOINT_SOCKET env default.
	WorkloadAPISocket string
	// TrustDomain is the deployment trust domain. The SPIRE server's own SVID
	// (spiffe://<td>/spire/server) is the ONLY identity the client authorizes as the
	// mTLS peer, so a MITM presenting any other SVID is rejected.
	TrustDomain string
}

// New builds the mTLS gRPC broker. The client identity is the orchestrator's own
// SVID from the SPIFFE Workload API (its SPIFFE id must be in the server's
// admin_ids — the least-privilege delegated-admin credential); the server is
// authorized to be exactly spiffe://<td>/spire/server. The caller owns closing the
// Broker. A source/dial failure is returned so the selector can fail closed (nil
// broker ⇒ Entry API 503), never fall back to an unauthenticated path.
func New(ctx context.Context, cfg Config) (*Broker, error) {
	if cfg.Addr == "" {
		return nil, errors.New("empty SPIRE entry API address")
	}
	if cfg.TrustDomain == "" {
		return nil, errors.New("empty trust domain")
	}
	td, err := spiffeid.TrustDomainFromString(cfg.TrustDomain)
	if err != nil {
		return nil, fmt.Errorf("parse trust domain: %w", err)
	}
	serverID, err := spiffeid.FromSegments(td, "spire", "server")
	if err != nil {
		return nil, fmt.Errorf("derive SPIRE server id: %w", err)
	}

	var srcOpts []workloadapi.X509SourceOption
	if cfg.WorkloadAPISocket != "" {
		srcOpts = append(srcOpts, workloadapi.WithClientOptions(workloadapi.WithAddr(cfg.WorkloadAPISocket)))
	}
	// Blocks until the orchestrator has its first SVID or ctx is done — the caller
	// bounds ctx so a missing Workload API fails closed at startup rather than hanging.
	source, err := workloadapi.NewX509Source(ctx, srcOpts...)
	if err != nil {
		return nil, fmt.Errorf("obtain orchestrator SVID from workload API: %w", err)
	}

	// mTLS: present the orchestrator's SVID, trust the SPIRE trust bundle, and
	// authorize the peer as ONLY the SPIRE server's SVID.
	tlsCfg := tlsconfig.MTLSClientConfig(source, source, tlsconfig.AuthorizeID(serverID))
	conn, err := grpc.NewClient(cfg.Addr, grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg)))
	if err != nil {
		_ = source.Close()
		return nil, fmt.Errorf("dial SPIRE entry API %s: %w", cfg.Addr, err)
	}

	return &Broker{
		entries: entryv1.NewEntryClient(conn),
		agents:  agentv1.NewAgentClient(conn),
		closer: closeFunc(func() error {
			cerr := conn.Close()
			serr := source.Close()
			return errors.Join(cerr, serr)
		}),
	}, nil
}

// NewWithClients builds a Broker from already-constructed entry/agent clients. It
// is the seam the bufconn tests use (real generated clients over an in-memory
// connection) — no Workload API, no network.
func NewWithClients(entries EntryAPI, agents AgentAPI) *Broker {
	return &Broker{entries: entries, agents: agents}
}

// Close releases the underlying connection and SVID source. Safe on a
// client-injected Broker (no-op).
func (b *Broker) Close() error {
	if b.closer == nil {
		return nil
	}
	return b.closer.Close()
}

// CreateEntry registers ONE workload/bootstrap entry. It is idempotent: an
// identical entry that already exists (AlreadyExists) is success, matching the exec
// broker's "already exists" tolerance, so a reconcile-on-connect replay is a no-op.
func (b *Broker) CreateEntry(ctx context.Context, spec substrate.EntrySpec) error {
	spiffeID, err := parseSPIFFEID(spec.SPIFFEID)
	if err != nil {
		return fmt.Errorf("parse spiffe id: %w", err)
	}
	parentID, err := parseSPIFFEID(spec.ParentID)
	if err != nil {
		return fmt.Errorf("parse parent id: %w", err)
	}
	selectors, err := parseSelectors(spec.Selectors)
	if err != nil {
		return err
	}
	resp, err := b.entries.BatchCreateEntry(ctx, &entryv1.BatchCreateEntryRequest{
		Entries: []*types.Entry{{
			SpiffeId:  spiffeID,
			ParentId:  parentID,
			Selectors: selectors,
		}},
	})
	if err != nil {
		return fmt.Errorf("spire BatchCreateEntry: %w", err)
	}
	if len(resp.GetResults()) != 1 {
		return fmt.Errorf("spire BatchCreateEntry: expected 1 result, got %d", len(resp.GetResults()))
	}
	st := resp.GetResults()[0].GetStatus()
	switch codes.Code(st.GetCode()) {
	case codes.OK, codes.AlreadyExists:
		return nil
	default:
		return fmt.Errorf("spire BatchCreateEntry: %s: %s", codes.Code(st.GetCode()), st.GetMessage())
	}
}

// DeleteEntry removes EVERY entry for spiffeID (one SPIFFE id can have several
// entries — e.g. different parents left behind by a rotate). An absent entry is a
// no-op success (idempotent teardown), and a delete that races another delete
// (NotFound) is tolerated.
func (b *Broker) DeleteEntry(ctx context.Context, spiffeID string) error {
	id, err := parseSPIFFEID(spiffeID)
	if err != nil {
		return fmt.Errorf("parse spiffe id: %w", err)
	}
	// Server-side filter to just this SPIFFE id.
	ids, err := b.listEntryIDs(ctx, &entryv1.ListEntriesRequest_Filter{BySpiffeId: id}, nil)
	if err != nil {
		return err
	}
	return b.batchDelete(ctx, ids)
}

// DeleteEntriesUnderParent implements substrate.ParentPrefixDeleter: delete EVERY
// entry whose parent id begins with parentPrefix. The SPIRE ListEntries filter can
// only match an EXACT parent id, but the org node prefix's trailing segment is a
// per-cert fingerprint that varies — so we list and filter client-side on the
// parent-id STRING prefix, exactly as the exec broker does. Backs the rotate
// hygiene (EntryService.InvalidateOrgNodeEntries); the prefix is CP-derived from
// the caller's own org, so only that org's entries are ever swept.
func (b *Broker) DeleteEntriesUnderParent(ctx context.Context, parentPrefix string) error {
	if parentPrefix == "" {
		return errors.New("empty parent prefix")
	}
	keepIf := func(e *types.Entry) bool {
		return strings.HasPrefix(spiffeIDString(e.GetParentId()), parentPrefix)
	}
	ids, err := b.listEntryIDs(ctx, nil, keepIf)
	if err != nil {
		return err
	}
	return b.batchDelete(ctx, ids)
}

// EvictNodesUnderPrefix implements substrate.NodeEvicter: evict every attested
// agent (node) whose SPIFFE id begins with prefix, except keepNodeID, and return
// the count. Backs the rotate/disconnect hygiene (EntryService.EvictSupersededOrgNodes).
// The prefix is CP-derived from the caller's own org, so this can only ever reach
// that org's nodes — never another org's, never a platform node. An already-absent
// node (NotFound) is a no-op success.
func (b *Broker) EvictNodesUnderPrefix(ctx context.Context, prefix, keepNodeID string) (int, error) {
	if prefix == "" {
		return 0, errors.New("empty node prefix")
	}
	var pageToken string
	evicted := 0
	for {
		resp, err := b.agents.ListAgents(ctx, &agentv1.ListAgentsRequest{PageToken: pageToken})
		if err != nil {
			return evicted, fmt.Errorf("spire ListAgents: %w", err)
		}
		for _, ag := range resp.GetAgents() {
			id := spiffeIDString(ag.GetId())
			if id == keepNodeID || !strings.HasPrefix(id, prefix) {
				continue
			}
			if _, derr := b.agents.DeleteAgent(ctx, &agentv1.DeleteAgentRequest{Id: ag.GetId()}); derr != nil {
				if isNotFound(derr) {
					continue
				}
				return evicted, fmt.Errorf("spire DeleteAgent %s: %w", id, derr)
			}
			evicted++
		}
		pageToken = resp.GetNextPageToken()
		if pageToken == "" {
			return evicted, nil
		}
	}
}

// listEntryIDs pages through ListEntries applying an optional server-side filter
// and an optional client-side keep predicate, returning the matched entry ids.
func (b *Broker) listEntryIDs(ctx context.Context, filter *entryv1.ListEntriesRequest_Filter, keep func(*types.Entry) bool) ([]string, error) {
	var ids []string
	var pageToken string
	for {
		resp, err := b.entries.ListEntries(ctx, &entryv1.ListEntriesRequest{Filter: filter, PageToken: pageToken})
		if err != nil {
			return nil, fmt.Errorf("spire ListEntries: %w", err)
		}
		for _, e := range resp.GetEntries() {
			if keep != nil && !keep(e) {
				continue
			}
			if id := e.GetId(); id != "" {
				ids = append(ids, id)
			}
		}
		pageToken = resp.GetNextPageToken()
		if pageToken == "" {
			return ids, nil
		}
	}
}

// batchDelete deletes the given entry ids, tolerating an already-absent id
// (NotFound) as a no-op success so teardown is idempotent. An empty id list is a
// no-op (no RPC).
func (b *Broker) batchDelete(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	resp, err := b.entries.BatchDeleteEntry(ctx, &entryv1.BatchDeleteEntryRequest{Ids: ids})
	if err != nil {
		return fmt.Errorf("spire BatchDeleteEntry: %w", err)
	}
	for _, res := range resp.GetResults() {
		switch codes.Code(res.GetStatus().GetCode()) {
		case codes.OK, codes.NotFound:
			continue
		default:
			return fmt.Errorf("spire BatchDeleteEntry %s: %s: %s", res.GetId(), codes.Code(res.GetStatus().GetCode()), res.GetStatus().GetMessage())
		}
	}
	return nil
}

// parseSPIFFEID splits a "spiffe://<td>/<path>" string into the SDK's
// trust-domain + path form, using go-spiffe's validated parser so a malformed id
// is rejected rather than shipped to the server.
func parseSPIFFEID(s string) (*types.SPIFFEID, error) {
	id, err := spiffeid.FromString(s)
	if err != nil {
		return nil, err
	}
	return &types.SPIFFEID{TrustDomain: id.TrustDomain().Name(), Path: id.Path()}, nil
}

// spiffeIDString renders an SDK SPIFFEID back to its canonical string form for
// prefix comparison. A nil id renders empty (never matches a non-empty prefix).
func spiffeIDString(id *types.SPIFFEID) string {
	if id == nil || id.GetTrustDomain() == "" {
		return ""
	}
	return "spiffe://" + id.GetTrustDomain() + id.GetPath()
}

// parseSelectors converts "type:value" selector strings (e.g. "k8s:ns:foo") into
// the SDK Selector form, splitting on the FIRST colon so the value keeps its own
// colons ("ns:foo"). An empty selector list, or a selector missing a type/value,
// is rejected — the authz layer already required namespace+pod+SA, so a malformed
// selector reaching here is a bug, not a benign case.
func parseSelectors(sels []string) ([]*types.Selector, error) {
	if len(sels) == 0 {
		return nil, errors.New("entry has no selectors")
	}
	out := make([]*types.Selector, 0, len(sels))
	for _, s := range sels {
		i := strings.IndexByte(s, ':')
		if i <= 0 || i == len(s)-1 {
			return nil, fmt.Errorf("malformed selector %q (want type:value)", s)
		}
		out = append(out, &types.Selector{Type: s[:i], Value: s[i+1:]})
	}
	return out, nil
}

// isNotFound reports whether err is a gRPC NotFound status (an already-absent node
// on evict — a no-op success).
func isNotFound(err error) bool {
	return status.Code(err) == codes.NotFound
}

type closeFunc func() error

func (f closeFunc) Close() error { return f() }
