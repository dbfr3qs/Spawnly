import React, { useEffect, useState } from 'react';
import { View, Text, Switch, Button, StyleSheet, ActivityIndicator, Alert } from 'react-native';
import { getRequest, approve, deny, AlreadyHandledError, type ConsentRequest } from '../api';
import { confirmWithBiometrics } from '../biometric';
import { approveGuarded, toggleScope } from '../consent-logic';

export function RequestDetailScreen({ id, onDone }: { id: string; onDone: () => void }) {
  const [req, setReq] = useState<ConsentRequest | null>(null);
  const [selected, setSelected] = useState<string[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    // Always re-fetch authoritative state — never trust the push payload.
    getRequest(id)
      .then((r) => {
        setReq(r);
        setSelected(r.scopes); // default: full requested set
      })
      .catch((e) => {
        if (e instanceof AlreadyHandledError) {
          Alert.alert('Already handled', 'This request was resolved elsewhere.', [
            { text: 'OK', onPress: onDone },
          ]);
        } else {
          setError(e instanceof Error ? e.message : 'Failed to load.');
        }
      });
  }, [id, onDone]);

  if (error) return <Text style={styles.error}>{error}</Text>;
  if (!req) return <ActivityIndicator style={{ marginTop: 40 }} />;

  async function onApprove() {
    if (!req) return;
    setBusy(true);
    try {
      const submitted = await approveGuarded(
        { confirm: confirmWithBiometrics, submit: (scopes) => approve(req.id, scopes) },
        req.scopes,
        selected,
      );
      if (submitted) onDone();
      else setError('Approval requires biometric confirmation.');
    } catch (e) {
      setError(e instanceof Error ? e.message : 'Approve failed.');
    } finally {
      setBusy(false);
    }
  }

  async function onDeny() {
    if (!req) return;
    setBusy(true);
    try {
      await deny(req.id);
      onDone();
    } catch (e) {
      setError(e instanceof Error ? e.message : 'Deny failed.');
    } finally {
      setBusy(false);
    }
  }

  return (
    <View style={styles.container}>
      <Text style={styles.edge}>
        {req.parentType} → {req.childType}
      </Text>
      {req.bindingMessage ? <Text style={styles.binding}>{req.bindingMessage}</Text> : null}

      <Text style={styles.section}>Requested scopes</Text>
      {req.scopes.map((scope) => (
        <View key={scope} style={styles.scopeRow}>
          <Switch
            value={selected.includes(scope)}
            onValueChange={() => setSelected((cur) => toggleScope(req.scopes, cur, scope))}
          />
          <Text style={styles.scope}>{scope}</Text>
        </View>
      ))}

      {busy ? (
        <ActivityIndicator style={{ marginTop: 20 }} />
      ) : (
        <View style={styles.actions}>
          <Button title="Approve" onPress={onApprove} disabled={selected.length === 0} />
          <Button title="Deny" color="#c0392b" onPress={onDeny} />
        </View>
      )}
      {error ? <Text style={styles.error}>{error}</Text> : null}
    </View>
  );
}

const styles = StyleSheet.create({
  container: { flex: 1, padding: 20, gap: 8 },
  edge: { fontSize: 22, fontWeight: '700' },
  binding: { fontSize: 15, color: '#444', fontStyle: 'italic', marginTop: 4 },
  section: { fontSize: 14, fontWeight: '600', color: '#666', marginTop: 16 },
  scopeRow: { flexDirection: 'row', alignItems: 'center', gap: 10, paddingVertical: 4 },
  scope: { fontSize: 16 },
  actions: { flexDirection: 'row', justifyContent: 'space-between', marginTop: 28 },
  error: { color: '#c0392b', marginTop: 12 },
});
