package main

import (
	"context"
	"strings"
	"testing"
)

// TestExecBroker_DeleteEntriesUnderParent verifies the rotate-sweep mechanism:
// list all entries, delete ONLY those whose Parent ID matches the org-node prefix.
func TestExecBroker_DeleteEntriesUnderParent(t *testing.T) {
	show := `Entry ID         : e-old-bootstrap
SPIFFE ID        : spiffe://cluster.local/org/acme/substrate-agent
Parent ID        : spiffe://cluster.local/spire/agent/x509pop/org/acme/OLDFP
Selector         : k8s:ns:substrate-sim

Entry ID         : e-old-workload
SPIFFE ID        : spiffe://cluster.local/org/acme/agent/worker/agent-1
Parent ID        : spiffe://cluster.local/spire/agent/x509pop/org/acme/OLDFP
Selector         : k8s:ns:substrate-sim

Entry ID         : e-other-org
SPIFFE ID        : spiffe://cluster.local/org/globex/agent/worker/agent-9
Parent ID        : spiffe://cluster.local/spire/agent/x509pop/org/globex/GFP
Selector         : k8s:ns:substrate-sim`

	var deleted []string
	broker := &execEntryBroker{
		bin: "spire-server",
		runCmd: func(_ context.Context, _ string, args ...string) (string, error) {
			joined := strings.Join(args, " ")
			switch {
			case strings.Contains(joined, "entry show"):
				return show, nil
			case strings.Contains(joined, "entry delete"):
				// -entryID <id> is the last pair.
				for i, a := range args {
					if a == "-entryID" && i+1 < len(args) {
						deleted = append(deleted, args[i+1])
					}
				}
				return "Deleted entry ID: ok", nil
			}
			return "", nil
		},
	}

	prefix := "spiffe://cluster.local/spire/agent/x509pop/org/acme/"
	if err := broker.DeleteEntriesUnderParent(context.Background(), prefix); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	// Only acme's two entries (old bootstrap + old workload) are swept; globex's is untouched.
	got := map[string]bool{}
	for _, id := range deleted {
		got[id] = true
	}
	if !got["e-old-bootstrap"] || !got["e-old-workload"] {
		t.Fatalf("did not sweep acme's node entries: %v", deleted)
	}
	if got["e-other-org"] {
		t.Fatal("swept another org's entry — org isolation breach in the rotate sweep")
	}
	if len(deleted) != 2 {
		t.Fatalf("deleted %d entries, want exactly 2 (acme's)", len(deleted))
	}
}

// TestExecBroker_EvictNodesUnderPrefix verifies the node-side rotate hygiene:
// evict the org's SUPERSEDED x509pop nodes, keep the node the new leaf attests
// as, and never touch another org's (or the platform's) nodes.
func TestExecBroker_EvictNodesUnderPrefix(t *testing.T) {
	list := `SPIFFE ID         : spiffe://cluster.local/spire/agent/x509pop/org/acme/OLDFP1
Attestation type  : x509pop
Expiration time   : 2026-07-21 06:36:32 +0000 UTC

SPIFFE ID         : spiffe://cluster.local/spire/agent/x509pop/org/acme/OLDFP2
Attestation type  : x509pop

SPIFFE ID         : spiffe://cluster.local/spire/agent/x509pop/org/acme/NEWFP
Attestation type  : x509pop

SPIFFE ID         : spiffe://cluster.local/spire/agent/x509pop/org/globex/GFP
Attestation type  : x509pop

SPIFFE ID         : spiffe://cluster.local/spire/agent/k8s_psat/agent-platform/NODEUID
Attestation type  : k8s_psat`

	var evicted []string
	broker := &execEntryBroker{
		bin: "spire-server",
		runCmd: func(_ context.Context, _ string, args ...string) (string, error) {
			joined := strings.Join(args, " ")
			switch {
			case strings.Contains(joined, "agent list"):
				return list, nil
			case strings.Contains(joined, "agent evict"):
				for i, a := range args {
					if a == "-spiffeID" && i+1 < len(args) {
						evicted = append(evicted, args[i+1])
					}
				}
				return "Agent evicted successfully", nil
			}
			return "", nil
		},
	}

	prefix := "spiffe://cluster.local/spire/agent/x509pop/org/acme/"
	keep := prefix + "NEWFP"
	n, err := broker.EvictNodesUnderPrefix(context.Background(), prefix, keep)
	if err != nil {
		t.Fatalf("evict: %v", err)
	}
	if n != 2 {
		t.Fatalf("reported %d evictions, want 2 (the count is what the connect audits)", n)
	}
	got := map[string]bool{}
	for _, id := range evicted {
		got[id] = true
	}
	if !got[prefix+"OLDFP1"] || !got[prefix+"OLDFP2"] {
		t.Fatalf("did not evict acme's superseded nodes: %v", evicted)
	}
	if got[keep] {
		t.Fatal("evicted the node the NEW leaf attests as — the connect would never come online")
	}
	for _, forbidden := range []string{
		"spiffe://cluster.local/spire/agent/x509pop/org/globex/GFP",
		"spiffe://cluster.local/spire/agent/k8s_psat/agent-platform/NODEUID",
	} {
		if got[forbidden] {
			t.Fatalf("evicted a node outside the org prefix (%s) — isolation breach", forbidden)
		}
	}
	if len(evicted) != 2 {
		t.Fatalf("evicted %d nodes, want exactly 2 (acme's superseded)", len(evicted))
	}
}

// An empty prefix must never be treated as "everything".
func TestExecBroker_EvictNodesUnderPrefix_RefusesEmptyPrefix(t *testing.T) {
	broker := &execEntryBroker{bin: "spire-server", runCmd: func(context.Context, string, ...string) (string, error) {
		t.Fatal("must not shell out for an empty prefix")
		return "", nil
	}}
	if _, err := broker.EvictNodesUnderPrefix(context.Background(), "", ""); err == nil {
		t.Fatal("empty prefix must error, not sweep every node")
	}
}
