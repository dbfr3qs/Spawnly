import * as LocalAuthentication from 'expo-local-authentication';

// confirmWithBiometrics prompts for Face ID / Touch ID / fingerprint and
// resolves true only on a successful local authentication. When the device has
// no enrolled biometrics we fall back to the device passcode (the OS prompt
// handles this); if neither is available we return false so the caller refuses
// the action rather than silently approving on an unattended phone.
export async function confirmWithBiometrics(reason: string): Promise<boolean> {
  const hasHardware = await LocalAuthentication.hasHardwareAsync();
  const enrolled = await LocalAuthentication.isEnrolledAsync();
  if (!hasHardware || !enrolled) return false;
  const result = await LocalAuthentication.authenticateAsync({
    promptMessage: reason,
    cancelLabel: 'Cancel',
    disableDeviceFallback: false,
  });
  return result.success;
}
