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
