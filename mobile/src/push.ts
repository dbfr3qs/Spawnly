import { Platform } from 'react-native';
import * as Notifications from 'expo-notifications';
import { registerDevice } from './api';

// Show an alert banner even when the app is foregrounded.
Notifications.setNotificationHandler({
  handleNotification: async () => ({
    shouldShowAlert: true,
    shouldPlaySound: true,
    shouldSetBadge: false,
  }),
});

// registerForPush asks for OS notification permission, obtains the NATIVE device
// push token (raw FCM token on Android, APNs token on iOS — we use direct
// FCM/APNs, not Expo's push service), and registers it with the gateway against
// the authenticated user. Returns false if permission was denied.
export async function registerForPush(): Promise<boolean> {
  const settings = await Notifications.getPermissionsAsync();
  let status = settings.status;
  if (status !== 'granted') {
    status = (await Notifications.requestPermissionsAsync()).status;
  }
  if (status !== 'granted') return false;

  const devicePushToken = await Notifications.getDevicePushTokenAsync();
  const platform = Platform.OS === 'ios' ? 'ios' : 'android';
  await registerDevice(platform, String(devicePushToken.data));
  return true;
}

// The consentRequestId carried in a push, used to deep-link to the detail
// screen. The app re-fetches the authoritative request — the payload is only a
// pointer, never trusted state.
export function consentRequestIdFrom(
  notification: Notifications.Notification,
): string | undefined {
  const data = notification.request.content.data as Record<string, unknown> | undefined;
  const id = data?.consentRequestId;
  return typeof id === 'string' ? id : undefined;
}
