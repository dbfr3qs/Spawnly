import React, { useEffect, useState } from 'react';
import { View, Text, Button, StyleSheet, Switch } from 'react-native';
import { logout } from '../auth';
import { registerForPush } from '../push';
import { config } from '../config';

export function SettingsScreen({ onLoggedOut }: { onLoggedOut: () => void }) {
  const [pushOn, setPushOn] = useState(false);

  useEffect(() => {
    // Re-register the push token on entry (no-op server-side if unchanged).
    registerForPush().then(setPushOn).catch(() => setPushOn(false));
  }, []);

  return (
    <View style={styles.container}>
      <View style={styles.row}>
        <Text style={styles.label}>Push notifications</Text>
        <Switch
          value={pushOn}
          onValueChange={async (next) => {
            if (next) setPushOn(await registerForPush());
          }}
        />
      </View>
      <Text style={styles.meta}>Gateway: {config.gatewayUrl}</Text>
      <Text style={styles.meta}>Issuer: {config.issuer}</Text>
      <View style={{ marginTop: 24 }}>
        <Button
          title="Log out"
          color="#c0392b"
          onPress={async () => {
            await logout();
            onLoggedOut();
          }}
        />
      </View>
    </View>
  );
}

const styles = StyleSheet.create({
  container: { flex: 1, padding: 20, gap: 12 },
  row: { flexDirection: 'row', justifyContent: 'space-between', alignItems: 'center' },
  label: { fontSize: 16 },
  meta: { fontSize: 12, color: '#777' },
});
