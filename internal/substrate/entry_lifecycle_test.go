package substrate

import (
	"context"
	"testing"

	"github.com/spawnly/platform/internal/substrate/devca"
)

// prefixBroker is a fakeBroker-equivalent that ALSO implements
// ParentPrefixDeleter, capturing prefix sweeps.
type prefixBroker struct {
	created      []EntrySpec
	deleted      []string
	sweptPrefix  []string
	supportsSwep bool
}

func (b *prefixBroker) CreateEntry(_ context.Context, s EntrySpec) error { b.created = append(b.created, s); return nil }
func (b *prefixBroker) DeleteEntry(_ context.Context, id string) error   { b.deleted = append(b.deleted, id); return nil }
func (b *prefixBroker) DeleteEntriesUnderParent(_ context.Context, prefix string) error {
	b.sweptPrefix = append(b.sweptPrefix, prefix)
	return nil
}

// A broker WITHOUT the prefix capability, to prove InvalidateOrgNodeEntries fails
// closed (returns an error the caller renders best-effort) rather than silently
// skipping the rotate hygiene.
type plainBroker struct{ deleted []string }

func (b *plainBroker) CreateEntry(context.Context, EntrySpec) error { return nil }
func (b *plainBroker) DeleteEntry(_ context.Context, id string) error {
	b.deleted = append(b.deleted, id)
	return nil
}

func TestInvalidateOrgNodeEntries_SweepsOrgNodePrefix(t *testing.T) {
	broker := &prefixBroker{}
	svc := &EntryService{Broker: broker, TrustDomain: "cluster.local"}
	if err := svc.InvalidateOrgNodeEntries(context.Background(), "Acme"); err != nil {
		t.Fatalf("invalidate: %v", err)
	}
	wantPrefix, _ := devca.NodeSpiffePathPrefix("cluster.local", "acme")
	if len(broker.sweptPrefix) != 1 || broker.sweptPrefix[0] != wantPrefix {
		t.Fatalf("swept = %v, want [%s]", broker.sweptPrefix, wantPrefix)
	}
}

func TestInvalidateOrgNodeEntries_FailsClosedWithoutCapability(t *testing.T) {
	svc := &EntryService{Broker: &plainBroker{}, TrustDomain: "cluster.local"}
	if err := svc.InvalidateOrgNodeEntries(context.Background(), "acme"); err == nil {
		t.Fatal("want an error when the broker cannot enumerate entries, got nil")
	}
}

func TestInvalidateOrgNodeEntries_MisconfiguredTrustDomainRefuses(t *testing.T) {
	svc := &EntryService{Broker: &prefixBroker{}, TrustDomain: ""}
	if err := svc.InvalidateOrgNodeEntries(context.Background(), "acme"); err == nil {
		t.Fatal("want refusal when trust domain is empty")
	}
}

func TestInvalidateOrgNodeEntries_BadOrgRefused(t *testing.T) {
	svc := &EntryService{Broker: &prefixBroker{}, TrustDomain: "cluster.local"}
	if err := svc.InvalidateOrgNodeEntries(context.Background(), "bad/org"); err == nil {
		t.Fatal("want refusal for an org id that is not a valid path token")
	}
}

// evictBroker implements NodeEvicter, capturing node-eviction sweeps.
type evictBroker struct {
	prefixBroker
	evicted [][2]string // {prefix, keep}
}

func (b *evictBroker) EvictNodesUnderPrefix(_ context.Context, prefix, keep string) (int, error) {
	b.evicted = append(b.evicted, [2]string{prefix, keep})
	return len(b.evicted), nil
}

// The node-side rotate hygiene derives the sweep prefix from the SERVER-AUTHORITATIVE
// org (normalized), so it can only ever reach that org's own x509pop nodes, and it
// passes through the node to KEEP (the one the freshly-issued leaf attests as).
func TestEvictSupersededOrgNodes_ScopedToOrgPrefix(t *testing.T) {
	broker := &evictBroker{}
	svc := &EntryService{Broker: broker, TrustDomain: "cluster.local"}
	wantPrefix, _ := devca.NodeSpiffePathPrefix("cluster.local", "acme")
	keep := wantPrefix + "NEWFP"

	if _, err := svc.EvictSupersededOrgNodes(context.Background(), "Acme", keep); err != nil {
		t.Fatalf("evict: %v", err)
	}
	if len(broker.evicted) != 1 || broker.evicted[0][0] != wantPrefix || broker.evicted[0][1] != keep {
		t.Fatalf("evicted = %v, want [[%s %s]]", broker.evicted, wantPrefix, keep)
	}
}

func TestEvictSupersededOrgNodes_FailsClosedWithoutCapability(t *testing.T) {
	svc := &EntryService{Broker: &plainBroker{}, TrustDomain: "cluster.local"}
	if _, err := svc.EvictSupersededOrgNodes(context.Background(), "acme", ""); err == nil {
		t.Fatal("want an error when the broker cannot evict nodes, got nil")
	}
}

func TestEvictSupersededOrgNodes_RefusesMisconfiguredOrInvalidOrg(t *testing.T) {
	if _, err := (&EntryService{Broker: &evictBroker{}, TrustDomain: ""}).
		EvictSupersededOrgNodes(context.Background(), "acme", ""); err == nil {
		t.Fatal("want refusal when trust domain is empty")
	}
	if _, err := (&EntryService{Broker: &evictBroker{}, TrustDomain: "cluster.local"}).
		EvictSupersededOrgNodes(context.Background(), "../platform", ""); err == nil {
		t.Fatal("want refusal for a malformed org id (never a traversal-shaped prefix)")
	}
}
