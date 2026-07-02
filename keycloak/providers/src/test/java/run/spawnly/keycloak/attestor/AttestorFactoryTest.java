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
        Mockito.when(scope.get("attestor")).thenReturn(attestor);
        Mockito.when(scope.get("spireJwksUrl")).thenReturn(null);
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
    void unknownAttestor_hardFails() {
        IllegalStateException ex = assertThrows(IllegalStateException.class,
                () -> AttestorFactory.build(scopeWith("nonsense")));
        // The message names the offending value, like the C# InvalidOperationException.
        org.junit.jupiter.api.Assertions.assertTrue(ex.getMessage().contains("nonsense"));
    }
}
