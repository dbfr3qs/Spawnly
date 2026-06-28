// Dynamic Expo config layered on app.json. Its only job is to add DEV-ONLY
// cleartext exceptions so a local dev build can reach the port-forwarded
// gateway + IdentityServer over plain HTTP (iOS ATS and Android both block
// cleartext by default). Production builds DON'T set EXPO_PUBLIC_ALLOW_CLEARTEXT,
// so they keep app.json's HTTPS-only posture untouched.
//
// Endpoint values (issuer/gateway/clientId/auth+token endpoints) are read at
// runtime by src/config.ts from EXPO_PUBLIC_* (inlined by Metro), so they do not
// need to be plumbed through here.
const allowCleartext = process.env.EXPO_PUBLIC_ALLOW_CLEARTEXT === 'true';

module.exports = ({ config }) => {
  if (!allowCleartext) return config;
  return {
    ...config,
    ios: {
      ...config.ios,
      infoPlist: {
        ...config.ios?.infoPlist,
        // Allow plain-HTTP to local/LAN hosts only (the dev port-forwards).
        NSAppTransportSecurity: { NSAllowsLocalNetworking: true },
      },
    },
    android: {
      ...config.android,
      usesCleartextTraffic: true,
    },
  };
};
