import React, { useState } from 'react';
import { View, Text, Button, StyleSheet, ActivityIndicator } from 'react-native';
import { login } from '../auth';

export function LoginScreen({ onLoggedIn }: { onLoggedIn: () => void }) {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  async function onPress() {
    setBusy(true);
    setError(null);
    try {
      if (await login()) onLoggedIn();
      else setError('Login was cancelled.');
    } catch (e) {
      setError(e instanceof Error ? e.message : 'Login failed.');
    } finally {
      setBusy(false);
    }
  }

  return (
    <View style={styles.container}>
      <Text style={styles.title}>Spawnly</Text>
      <Text style={styles.subtitle}>Approve agent authorizations from your phone.</Text>
      {busy ? <ActivityIndicator /> : <Button title="Log in" onPress={onPress} />}
      {error ? <Text style={styles.error}>{error}</Text> : null}
    </View>
  );
}

const styles = StyleSheet.create({
  container: { flex: 1, alignItems: 'center', justifyContent: 'center', padding: 24, gap: 16 },
  title: { fontSize: 32, fontWeight: '700' },
  subtitle: { fontSize: 16, color: '#555', textAlign: 'center' },
  error: { color: '#c0392b', marginTop: 12 },
});
