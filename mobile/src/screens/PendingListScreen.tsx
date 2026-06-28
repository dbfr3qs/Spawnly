import React, { useCallback, useEffect, useState } from 'react';
import { View, Text, FlatList, TouchableOpacity, StyleSheet, RefreshControl } from 'react-native';
import { listPending, type ConsentRequest } from '../api';
import { subscribeStream } from '../sse';

export function PendingListScreen({ onOpen }: { onOpen: (id: string) => void }) {
  const [items, setItems] = useState<ConsentRequest[]>([]);
  const [refreshing, setRefreshing] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const refresh = useCallback(async () => {
    setRefreshing(true);
    try {
      setItems(await listPending());
      setError(null);
    } catch (e) {
      setError(e instanceof Error ? e.message : 'Failed to load.');
    } finally {
      setRefreshing(false);
    }
  }, []);

  useEffect(() => {
    refresh();
    // Live updates: a streamed consent event refreshes the list (and the push
    // deep-link opens the detail directly). The payload is only a trigger — the
    // authoritative list is re-fetched.
    const unsubscribe = subscribeStream(() => {
      refresh();
    });
    return unsubscribe;
  }, [refresh]);

  return (
    <View style={styles.container}>
      {error ? <Text style={styles.error}>{error}</Text> : null}
      <FlatList
        data={items}
        keyExtractor={(it) => it.id}
        refreshControl={<RefreshControl refreshing={refreshing} onRefresh={refresh} />}
        ListEmptyComponent={<Text style={styles.empty}>No pending approvals.</Text>}
        renderItem={({ item }) => (
          <TouchableOpacity style={styles.card} onPress={() => onOpen(item.id)}>
            <Text style={styles.edge}>
              {item.parentType} → {item.childType}
            </Text>
            <Text style={styles.meta}>{item.scopes.length} scope(s) requested</Text>
          </TouchableOpacity>
        )}
      />
    </View>
  );
}

const styles = StyleSheet.create({
  container: { flex: 1, padding: 12 },
  card: { padding: 16, borderRadius: 10, backgroundColor: '#f3f4f6', marginBottom: 10 },
  edge: { fontSize: 18, fontWeight: '600' },
  meta: { fontSize: 14, color: '#555', marginTop: 4 },
  empty: { textAlign: 'center', color: '#777', marginTop: 40 },
  error: { color: '#c0392b', padding: 8 },
});
