package registry

import "testing"

// ValidPlacement must accept EVERY Placement* constant. This is the invariant that
// actually broke: the registry's preregister handler open-coded the allow-list,
// omitted PlacementSubstrate when it was added, and silently 400'd every
// substrate-placed spawn — a failure no unit test caught because nothing tied the
// validator to the constants. Adding a new placement without listing it here (and
// in ValidPlacement) now fails loudly instead.
func TestValidPlacement(t *testing.T) {
	// Every placement the package defines, plus "" (unset ⇒ k8s).
	valid := []string{"", PlacementK8s, PlacementRemote, PlacementSubstrate}
	for _, p := range valid {
		if !ValidPlacement(p) {
			t.Errorf("ValidPlacement(%q) = false, want true", p)
		}
	}

	for _, p := range []string{"k8", "K8s", "Substrate", "remote ", "bogus", "substrate/x"} {
		if ValidPlacement(p) {
			t.Errorf("ValidPlacement(%q) = true, want false (unknown placements must fail closed)", p)
		}
	}
}
