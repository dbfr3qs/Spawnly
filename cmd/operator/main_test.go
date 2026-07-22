// cmd/operator/main_test.go
package main

import "testing"

// cacheOptionsForNamespace is the isolation boundary between the platform
// operator (WATCH_NAMESPACE=default) and the substrate operator
// (WATCH_NAMESPACE=substrate-sim): each must reconcile ONLY its own namespace so
// they never race over the same AgentWorkload. An empty value must keep the
// historical cluster-wide behavior (an empty DefaultNamespaces map = all
// namespaces) so a stock deploy without the env behaves exactly as before.
func TestCacheOptionsForNamespace(t *testing.T) {
	t.Run("empty is cluster-wide", func(t *testing.T) {
		opts := cacheOptionsForNamespace("")
		if opts.DefaultNamespaces != nil {
			t.Fatalf("empty WATCH_NAMESPACE must leave DefaultNamespaces nil (cluster-wide), got %#v", opts.DefaultNamespaces)
		}
	})

	for _, ns := range []string{"default", "substrate-sim"} {
		t.Run("scoped to "+ns, func(t *testing.T) {
			opts := cacheOptionsForNamespace(ns)
			if opts.DefaultNamespaces == nil {
				t.Fatalf("WATCH_NAMESPACE=%q must scope the cache, got nil DefaultNamespaces", ns)
			}
			if len(opts.DefaultNamespaces) != 1 {
				t.Fatalf("WATCH_NAMESPACE=%q must scope to exactly one namespace, got %d: %#v", ns, len(opts.DefaultNamespaces), opts.DefaultNamespaces)
			}
			if _, ok := opts.DefaultNamespaces[ns]; !ok {
				t.Fatalf("WATCH_NAMESPACE=%q must scope to that namespace, got %#v", ns, opts.DefaultNamespaces)
			}
		})
	}
}
