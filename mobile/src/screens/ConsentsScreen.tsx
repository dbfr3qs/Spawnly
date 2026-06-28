import React, { useCallback, useEffect, useState } from 'react';
import { View, Text, FlatList, Button, StyleSheet, RefreshControl, Alert } from 'react-native';
import { listConsents, revokeConsent, type ConsentRecord } from '../api';

export function ConsentsScreen() {
  const [items, setItems] = useState<ConsentRecord[]>([]);
  const [refreshing, setRefreshing] = useState(false);

  const refresh = useCallback(async () => {
    setRefreshing(true);
    try {
      setItems(await listConsents());
    } catch {
      // surfaced via empty state
    } finally {
      setRefreshing(false);
    }
  }, []);

  useEffect(() => {
    refresh();
  }, [refresh]);

  async function onRevoke(rec: ConsentRecord) {
    Alert.alert('Revoke consent?', `${rec.parentType} → ${rec.childType}`, [
      { text: 'Cancel', style: 'cancel' },
      {
        text: 'Revoke',
        style: 'destructive',
        onPress: async () => {
          try {
            await revokeConsent(rec.id);
            await refresh();
          } catch (e) {
            Alert.alert('Revoke failed', e instanceof Error ? e.message : 'Unknown error');
          }
        },
      },
    ]);
  }

  return (
    <FlatList
      style={styles.list}
      data={items.filter((c) => !c.revoked)}
      keyExtractor={(it) => it.id}
      refreshControl={<RefreshControl refreshing={refreshing} onRefresh={refresh} />}
      ListEmptyComponent={<Text style={styles.empty}>No standing consents.</Text>}
      renderItem={({ item }) => (
        <View style={styles.card}>
          <Text style={styles.edge}>
            {item.parentType} → {item.childType}
          </Text>
          <Text style={styles.meta}>{(item.scopes ?? []).join(', ') || 'no scopes'}</Text>
          <Button title="Revoke" color="#c0392b" onPress={() => onRevoke(item)} />
        </View>
      )}
    />
  );
}

const styles = StyleSheet.create({
  list: { flex: 1, padding: 12 },
  card: { padding: 16, borderRadius: 10, backgroundColor: '#f3f4f6', marginBottom: 10, gap: 6 },
  edge: { fontSize: 17, fontWeight: '600' },
  meta: { fontSize: 13, color: '#555' },
  empty: { textAlign: 'center', color: '#777', marginTop: 40 },
});
