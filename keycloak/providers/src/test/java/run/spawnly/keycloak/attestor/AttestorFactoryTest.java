package run.spawnly.keycloak.attestor;

import org.junit.jupiter.api.Test;
import org.keycloak.Config;
import org.mockito.Mockito;

import static org.junit.jupiter.api.Assertions.assertInstanceOf;
import static org.junit.jupiter.api.Assertions.assertThrows;

/**
 * The ATTESTOR selector, mirroring identityserver/Program.cs: spiffe (or empty)
 * ⇒ SPIFFE verifier; unknown ⇒ hard fail (never a silent insecure fallback).
 */
class AttestorFactoryTest {

    private Config.Scope scopeWith(String attestor) {
        Config.Scope scope = Mockito.mock(Config.Scope.class);
        // Return null for any unstubbed key (env fallback / default kicks in).
        Mockito.when(scope.get(Mockito.anyString())).thenReturn(null);
        Mockito.when(scope.get("attestor")).thenReturn(attestor);
        return scope;
    }

    /**
     * A scope with the STSWEB_* fields set via SPI config, so aws-stsweb can be
     * constructed without relying on env in the test JVM (STSWEB_ISSUER etc.).
     */
    private Config.Scope stswebScope() {
        Config.Scope scope = Mockito.mock(Config.Scope.class);
        Mockito.when(scope.get(Mockito.anyString())).thenReturn(null);
        Mockito.when(scope.get("attestor")).thenReturn("aws-stsweb");
        Mockito.when(scope.get("stswebIssuer")).thenReturn("https://oidc.eks.example/id/ABCD");
        Mockito.when(scope.get("stswebJwksUrl")).thenReturn("https://oidc.eks.example/id/ABCD/jwks");
        return scope;
    }

    @Test
    void spiffe_selectsSpireVerifier() {
        assertInstanceOf(SpireCredentialVerifier.class, AttestorFactory.build(scopeWith("spiffe")));
    }

    @Test
    void empty_defaultsToSpire() {
        assertInstanceOf(SpireCredentialVerifier.class, AttestorFactory.build(scopeWith("")));
    }

    @Test
    void nullConfig_defaultsToSpire() {
        // No SPI config + no ATTESTOR env in the test JVM ⇒ default "spiffe".
        assertInstanceOf(SpireCredentialVerifier.class, AttestorFactory.build(null));
    }

    @Test
    void awsSts_selectsStsVerifier() {
        assertInstanceOf(StsCredentialVerifier.class, AttestorFactory.build(scopeWith("aws-sts")));
    }

    @Test
    void awsStsweb_selectsStsWebVerifier() {
        assertInstanceOf(StsWebCredentialVerifier.class, AttestorFactory.build(stswebScope()));
    }

    @Test
    void stsWebOptions_deriveJwksFromIssuerWhenUnset() {
        // Program.cs parity: STSWEB_JWKS_URL empty + STSWEB_ISSUER set ⇒
        // jwksUrl = issuer + "/.well-known/jwks.json"; audience defaults to spawnly.
        Config.Scope scope = Mockito.mock(Config.Scope.class);
        Mockito.when(scope.get(Mockito.anyString())).thenReturn(null);
        Mockito.when(scope.get("stswebIssuer")).thenReturn("https://oidc.eks.example/id/ABCD/");
        StsWebOptions o = AttestorFactory.stsWebOptions(scope);
        org.junit.jupiter.api.Assertions.assertEquals(
                "https://oidc.eks.example/id/ABCD/.well-known/jwks.json", o.jwksUrl());
        org.junit.jupiter.api.Assertions.assertEquals("spawnly", o.audience());
    }

    @Test
    void unknownAttestor_hardFails() {
        IllegalStateException ex = assertThrows(IllegalStateException.class,
                () -> AttestorFactory.build(scopeWith("nonsense")));
        // The message names the offending value, like the C# InvalidOperationException.
        org.junit.jupiter.api.Assertions.assertTrue(ex.getMessage().contains("nonsense"));
    }
}
