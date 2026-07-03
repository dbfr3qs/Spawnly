package run.spawnly.keycloak.ciba;

import org.junit.jupiter.api.Test;

import java.util.List;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertTrue;

class ConsentRequestTrackerTest {

    private static ConsentRequestTracker.Entry entry(String authResultId, long expiresAtMs) {
        return new ConsentRequestTracker.Entry(
                "realm-1", authResultId, "cr-" + authResultId, "alice",
                "travel-planner", "flight-search", "flight-search",
                List.of("flights:read"), expiresAtMs);
    }

    @Test
    void trackAndSnapshot() {
        ConsentRequestTracker t = new ConsentRequestTracker();
        t.track(entry("a", 0));
        t.track(entry("b", 0));
        assertEquals(2, t.snapshot().size());
    }

    @Test
    void untrackRemoves() {
        ConsentRequestTracker t = new ConsentRequestTracker();
        t.track(entry("a", 0));
        t.untrack("a");
        assertTrue(t.snapshot().isEmpty());
    }

    @Test
    void trackReplacesSameAuthResultId() {
        ConsentRequestTracker t = new ConsentRequestTracker();
        t.track(entry("a", 0));
        t.track(entry("a", 0));
        assertEquals(1, t.snapshot().size());
    }

    @Test
    void evictExpiredRemovesOnlyPastEntries() {
        ConsentRequestTracker t = new ConsentRequestTracker();
        long now = 1_000_000L;
        t.track(entry("past", now - 1));      // expired
        t.track(entry("future", now + 10_000)); // not yet
        t.track(entry("never", 0));            // 0 = no expiry, never swept

        int evicted = t.evictExpired(now);

        assertEquals(1, evicted);
        List<String> remaining = t.snapshot().stream().map(ConsentRequestTracker.Entry::authResultId).sorted().toList();
        assertEquals(List.of("future", "never"), remaining);
    }

    @Test
    void boundedAtCap() {
        ConsentRequestTracker t = new ConsentRequestTracker();
        for (int i = 0; i < ConsentRequestTracker.MAX_ENTRIES; i++) {
            t.track(entry("e" + i, 0));
        }
        assertEquals(ConsentRequestTracker.MAX_ENTRIES, t.snapshot().size());
        // One past the cap is dropped (not stored).
        t.track(entry("overflow", 0));
        assertEquals(ConsentRequestTracker.MAX_ENTRIES, t.snapshot().size());
        assertFalse(t.view().containsKey("overflow"));
    }
}
