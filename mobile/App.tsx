import React, { useEffect, useRef, useState } from 'react';
import { SafeAreaView, View, Text, Button, StyleSheet, ActivityIndicator } from 'react-native';
import { StatusBar } from 'expo-status-bar';
import * as Notifications from 'expo-notifications';
import { isLoggedIn } from './src/auth';
import { registerForPush, consentRequestIdFrom } from './src/push';
import { LoginScreen } from './src/screens/LoginScreen';
import { PendingListScreen } from './src/screens/PendingListScreen';
import { RequestDetailScreen } from './src/screens/RequestDetailScreen';
import { ConsentsScreen } from './src/screens/ConsentsScreen';
import { SettingsScreen } from './src/screens/SettingsScreen';

type Tab = 'pending' | 'consents' | 'settings';

// A deliberately small hand-rolled navigator keeps the dependency surface and
// the review footprint minimal; the flows are shallow (a tab bar plus a modal
// detail), so a full navigation library isn't warranted for v1.
export default function App() {
  const [ready, setReady] = useState(false);
  const [authed, setAuthed] = useState(false);
  const [tab, setTab] = useState<Tab>('pending');
  const [openId, setOpenId] = useState<string | null>(null);
  // Typed off the listener's return value so it's robust across expo-notifications
  // versions (Subscription vs EventSubscription).
  const responseListener = useRef<ReturnType<typeof Notifications.addNotificationResponseReceivedListener>>();

  useEffect(() => {
    isLoggedIn().then((ok) => {
      setAuthed(ok);
      setReady(true);
    });
  }, []);

  useEffect(() => {
    if (!authed) return;
    // Register for push once logged in.
    registerForPush().catch(() => {});
    // Tapping a push deep-links to the request detail (the id is only a pointer;
    // the detail screen re-fetches authoritative state).
    responseListener.current = Notifications.addNotificationResponseReceivedListener((resp) => {
      const id = consentRequestIdFrom(resp.notification);
      if (id) {
        setTab('pending');
        setOpenId(id);
      }
    });
    return () => responseListener.current?.remove();
  }, [authed]);

  if (!ready) {
    return (
      <SafeAreaView style={styles.center}>
        <ActivityIndicator />
      </SafeAreaView>
    );
  }

  if (!authed) {
    return (
      <SafeAreaView style={styles.flex}>
        <StatusBar style="auto" />
        <LoginScreen onLoggedIn={() => setAuthed(true)} />
      </SafeAreaView>
    );
  }

  if (openId) {
    return (
      <SafeAreaView style={styles.flex}>
        <StatusBar style="auto" />
        <View style={styles.header}>
          <Button title="‹ Back" onPress={() => setOpenId(null)} />
          <Text style={styles.headerTitle}>Approval</Text>
          <View style={{ width: 60 }} />
        </View>
        <RequestDetailScreen id={openId} onDone={() => setOpenId(null)} />
      </SafeAreaView>
    );
  }

  return (
    <SafeAreaView style={styles.flex}>
      <StatusBar style="auto" />
      <View style={styles.flex}>
        {tab === 'pending' && <PendingListScreen onOpen={setOpenId} />}
        {tab === 'consents' && <ConsentsScreen />}
        {tab === 'settings' && <SettingsScreen onLoggedOut={() => setAuthed(false)} />}
      </View>
      <View style={styles.tabbar}>
        <TabButton label="Pending" active={tab === 'pending'} onPress={() => setTab('pending')} />
        <TabButton label="Consents" active={tab === 'consents'} onPress={() => setTab('consents')} />
        <TabButton label="Settings" active={tab === 'settings'} onPress={() => setTab('settings')} />
      </View>
    </SafeAreaView>
  );
}

function TabButton({ label, active, onPress }: { label: string; active: boolean; onPress: () => void }) {
  return (
    <Text style={[styles.tab, active && styles.tabActive]} onPress={onPress}>
      {label}
    </Text>
  );
}

const styles = StyleSheet.create({
  flex: { flex: 1 },
  center: { flex: 1, alignItems: 'center', justifyContent: 'center' },
  header: { flexDirection: 'row', alignItems: 'center', justifyContent: 'space-between', padding: 12 },
  headerTitle: { fontSize: 17, fontWeight: '600' },
  tabbar: { flexDirection: 'row', borderTopWidth: StyleSheet.hairlineWidth, borderColor: '#ccc' },
  tab: { flex: 1, textAlign: 'center', paddingVertical: 14, color: '#888' },
  tabActive: { color: '#111', fontWeight: '700' },
});
