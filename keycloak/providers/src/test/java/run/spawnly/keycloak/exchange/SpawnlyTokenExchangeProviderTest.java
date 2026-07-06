package run.spawnly.keycloak.exchange;

import jakarta.ws.rs.WebApplicationException;
import jakarta.ws.rs.core.MultivaluedHashMap;
import jakarta.ws.rs.core.MultivaluedMap;
import jakarta.ws.rs.core.Response;
import org.junit.jupiter.api.BeforeEach;
import org.junit.jupiter.api.Test;
import org.keycloak.OAuthErrorException;
import org.keycloak.crypto.KeyUse;
import org.keycloak.crypto.KeyWrapper;
import org.keycloak.models.ClientModel;
import org.keycloak.models.KeyManager;
import org.keycloak.models.KeycloakContext;
import org.keycloak.models.KeycloakSession;
import org.keycloak.models.KeycloakUriInfo;
import org.keycloak.models.RealmModel;
import org.keycloak.models.TokenManager;
import org.keycloak.protocol.oidc.TokenExchangeContext;
import org.keycloak.representations.AccessToken;
import org.keycloak.services.cors.Cors;

import run.spawnly.keycloak.attestor.AgentCredentialVerifier;
import run.spawnly.keycloak.attestor.AgentIdentity;
import run.spawnly.keycloak.registry.AgentRecord;
import run.spawnly.keycloak.registry.AgentRegistryClient;
import run.spawnly.keycloak.registry.DelegationPolicy;

import java.net.URI;
import java.util.List;
import java.util.Map;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertNull;
import static org.junit.jupiter.api.Assertions.assertThrows;
import static org.junit.jupiter.api.Assertions.assertTrue;
import static org.mockito.ArgumentMatchers.any;
import static org.mockito.ArgumentMatchers.anyString;
import static org.mockito.ArgumentMatchers.eq;
import static org.mockito.Mockito.mock;
import static org.mockito.Mockito.when;

/**
 * Full policy matrix for the RFC 8693 token-exchange provider — the frozen
 * delegation semantics ported from TokenExchangeGrantValidator.cs. A mocked
 * registry drives every edge; a real RSA key mints/verifies subject_tokens so
 * the self-issued signature + issuer check runs for real.
 *
 * <p>Covers: allowed edge (+ wrapped act, scope, audience); denied edge; scope-
 * ceiling violation; depth at/over maxDepth; a revoked chain member at EACH
 * position (child, immediate parent, deeper ancestor); malformed/absent act;
 * foreign-issuer subject_token; missing scope; requested scope not in
 * subject_token; audience=delegation ⇒ token_use + no resource aud; token
 * typing/presence.
 */
class SpawnlyTokenExchangeProviderTest {

    // SPIFFE ids: <...>/agentType/agentId (agentId = last path segment).
    private static final String PARENT_SPIFFE = "spiffe://cluster.local/agent/t/alice/travel-planner/parent-1";
    private static final String CHILD_SPIFFE = "spiffe://cluster.local/agent/t/alice/flight-search/child-1";
    private static final String GRANDPARENT_SPIFFE =
            "spiffe://cluster.local/agent/t/alice/orchestrator/gp-1";

    private ExchangeTestFactory tf;
    private KeycloakSession session;
    private AgentRegistryClient registry;
    private Cors cors;
    private MultivaluedMap<String, String> form;
    private AccessToken minted; // captured from session.tokens().encode(...)

    @BeforeEach
    void setUp() throws Exception {
        tf = new ExchangeTestFactory();
        session = mock(KeycloakSession.class);
        registry = mock(AgentRegistryClient.class);
        cors = mock(Cors.class);
        form = new MultivaluedHashMap<>();
        minted = null;

        // Context wiring: session.getContext().getUri().getBaseUri() + realm name
        // → the realm issuer http://keycloak:8080/realms/spawnly.
        KeycloakContext kcCtx = mock(KeycloakContext.class);
        KeycloakUriInfo uriInfo = mock(KeycloakUriInfo.class);
        when(session.getContext()).thenReturn(kcCtx);
        when(kcCtx.getUri()).thenReturn(uriInfo);
        when(uriInfo.getBaseUri()).thenReturn(URI.create("http://keycloak:8080"));

        RealmModel realm = mock(RealmModel.class);
        when(realm.getName()).thenReturn("spawnly");

        // Realm SIG key: return a KeyWrapper wrapping the test public key for the
        // token's kid + RS256 (kid-specific lookup) so the signature verifies.
        KeyManager keys = mock(KeyManager.class);
        KeyWrapper kw = new KeyWrapper();
        kw.setKid(tf.kid);
        kw.setAlgorithm("RS256");
        kw.setUse(KeyUse.SIG);
        kw.setType("RSA");
        kw.setPublicKey(tf.key.toPublicKey());
        when(session.keys()).thenReturn(keys);
        when(keys.getKey(any(), eq(tf.kid), eq(KeyUse.SIG), eq("RS256"))).thenReturn(kw);

        // Capture the minted token; return a marker string (not re-inspected).
        TokenManager tokens = mock(TokenManager.class);
        when(session.tokens()).thenReturn(tokens);
        when(tokens.encode(any())).thenAnswer(inv -> {
            minted = inv.getArgument(0, AccessToken.class);
            return "signed.jwt.marker";
        });

        // cors.add(builder) returns a real Response so the success path completes.
        when(cors.add(any(Response.ResponseBuilder.class)))
                .thenAnswer(inv -> ((Response.ResponseBuilder) inv.getArgument(0)).build());

        this.realm = realm;
    }

    private RealmModel realm;

    // ── builders ────────────────────────────────────────────────────────────────

    private AgentCredentialVerifier actorVerifier(String spiffe, String agentId) {
        AgentCredentialVerifier v = mock(AgentCredentialVerifier.class);
        when(v.verify(anyString())).thenReturn(new AgentIdentity(agentId, spiffe, "spiffe-svid"));
        return v;
    }

    // Same scope→audience map the factory ships (mirrors deploy/keycloak/scopes.tf).
    private static final Map<String, String> SCOPE_AUDIENCE = Map.of(
            "sample-api-a:read", "sample-api-a",
            "sample-api-a:write", "sample-api-a",
            "sample-api-b:read", "sample-api-b",
            "sample-api-b:write", "sample-api-b",
            "fx:read", "travel-tools",
            "flights:read", "travel-tools",
            "hotels:read", "travel-tools",
            "registry.consent", "registry",
            "orchestrator:spawn", "orchestrator");

    private SpawnlyTokenExchangeProvider provider(AgentCredentialVerifier v) {
        return new SpawnlyTokenExchangeProvider(session, v, registry, SCOPE_AUDIENCE);
    }

    private TokenExchangeContext ctx(String subjectToken, String scope, String audience,
                                     String subjectType, String actorToken, String actorType) {
        TokenExchangeContext c = mock(TokenExchangeContext.class);
        TokenExchangeContext.Params params = mock(TokenExchangeContext.Params.class);
        when(params.getSubjectToken()).thenReturn(subjectToken);
        when(params.getSubjectTokenType()).thenReturn(subjectType);
        when(params.getActorToken()).thenReturn(actorToken);
        when(params.getActorTokenType()).thenReturn(actorType);
        when(params.getScope()).thenReturn(scope);
        when(params.getAudience()).thenReturn(audience == null ? List.of() : List.of(audience));
        when(c.getParams()).thenReturn(params);
        when(c.getCors()).thenReturn(cors);
        when(c.getRealm()).thenReturn(realm);
        when(c.getSession()).thenReturn(session);
        when(c.getFormParams()).thenReturn(form);
        ClientModel client = mock(ClientModel.class);
        when(client.getClientId()).thenReturn("flight-search");
        when(c.getClient()).thenReturn(client);
        return c;
    }

    private static final String ACCESS = "urn:ietf:params:oauth:token-type:access_token";
    private static final String JWT = "urn:ietf:params:oauth:token-type:jwt";

    /** A registry primed with an allowed travel-planner→flight-search edge (org-less). */
    private void allowedEdge(List<String> grantable, int maxDepth, String... activeIds) {
        when(registry.getAgent("parent-1"))
                .thenReturn(new AgentRecord("parent-1", "active", "alice", "travel-planner", null));
        when(registry.getAgent("child-1"))
                .thenReturn(new AgentRecord("child-1", "active", "alice", "flight-search", "parent-1"));
        when(registry.getDelegationPolicy("travel-planner", "flight-search"))
                .thenReturn(new DelegationPolicy(true, grantable, maxDepth));
    }

    /** Same allowed edge but the CHILD record belongs to org {@code childOrg}. */
    private void allowedEdgeChildOrg(List<String> grantable, int maxDepth, String childOrg) {
        when(registry.getAgent("parent-1"))
                .thenReturn(new AgentRecord("parent-1", "active", "alice", "travel-planner", null));
        when(registry.getAgent("child-1"))
                .thenReturn(new AgentRecord("child-1", "active", "alice", "flight-search", "parent-1", childOrg));
        when(registry.getDelegationPolicy("travel-planner", "flight-search"))
                .thenReturn(new DelegationPolicy(true, grantable, maxDepth));
    }

    // ── happy path ────────────────────────────────────────────────────────────

    @Test
    @SuppressWarnings("unchecked")
    void allowedEdge_wrapsActNarrowsScopeSetsAudience() throws Exception {
        allowedEdge(List.of("flights:read"), 0);
        String subject = tf.subjectToken("user:alice", "flights:read orchestrator:spawn",
                ExchangeTestFactory.act(PARENT_SPIFFE, null));
        TokenExchangeContext c = ctx(subject, "flights:read", null, ACCESS, "actor-svid", JWT);

        Response resp = provider(actorVerifier(CHILD_SPIFFE, "child-1")).exchange(c);
        assertEquals(200, resp.getStatus());

        // sub carried over unchanged.
        assertEquals("user:alice", minted.getSubject());
        // scope narrowed to exactly the requested set.
        assertEquals("flights:read", minted.getScope());
        // act = { sub: child, act: { sub: parent } } — child wraps prior chain.
        Map<String, Object> act = (Map<String, Object>) minted.getOtherClaims().get("act");
        assertEquals(CHILD_SPIFFE, act.get("sub"));
        Map<String, Object> inner = (Map<String, Object>) act.get("act");
        assertEquals(PARENT_SPIFFE, inner.get("sub"));
        // audience from the granted scope (flights:read → travel-tools).
        assertTrue(List.of(minted.getAudience()).contains("travel-tools"));
        // not a delegation token → no token_use.
        assertNull(minted.getOtherClaims().get("token_use"));
    }

    @Test
    void audienceDelegation_setsTokenUse_noResourceAud() throws Exception {
        allowedEdge(List.of("flights:read"), 0);
        String subject = tf.subjectToken("user:alice", "flights:read",
                ExchangeTestFactory.act(PARENT_SPIFFE, null));
        TokenExchangeContext c = ctx(subject, "flights:read", "delegation", ACCESS, "actor-svid", JWT);

        provider(actorVerifier(CHILD_SPIFFE, "child-1")).exchange(c);

        assertEquals("delegation", minted.getOtherClaims().get("token_use"));
        // No resource audience on a delegation-only token.
        assertNull(minted.getAudience());
    }

    // ── organization propagation (multi-tenancy) ─────────────────────────────────

    @Test
    void orgChild_stampsOrganizationFromChildRecord() throws Exception {
        // The exchanged token's org claim comes from the CHILD's registry record,
        // in the same array-of-aliases shape as user/agent tokens. Subject is in
        // the same org (the legitimate same-org delegation).
        allowedEdgeChildOrg(List.of("flights:read"), 0, "acme");
        String subject = tf.subjectTokenWithOrg("user:alice", "flights:read",
                ExchangeTestFactory.act(PARENT_SPIFFE, null), List.of("acme"));
        TokenExchangeContext c = ctx(subject, "flights:read", null, ACCESS, "actor-svid", JWT);

        Response resp = provider(actorVerifier(CHILD_SPIFFE, "child-1")).exchange(c);
        assertEquals(200, resp.getStatus());
        assertEquals(List.of("acme"), minted.getOtherClaims().get("organization"));
    }

    @Test
    void orgLessChild_noOrganizationClaim() throws Exception {
        // Org-less child + org-less subject ⇒ the exchanged token carries no org
        // claim (global chain, today's behaviour).
        allowedEdge(List.of("flights:read"), 0);
        String subject = tf.subjectToken("user:alice", "flights:read",
                ExchangeTestFactory.act(PARENT_SPIFFE, null));
        TokenExchangeContext c = ctx(subject, "flights:read", null, ACCESS, "actor-svid", JWT);

        Response resp = provider(actorVerifier(CHILD_SPIFFE, "child-1")).exchange(c);
        assertEquals(200, resp.getStatus());
        assertNull(minted.getOtherClaims().get("organization"));
    }

    @Test
    void crossOrgDelegation_isInvalidGrant() throws Exception {
        // Subject token names org acme, but the child record belongs to globex —
        // a cross-org delegation. Rejected with invalid_grant; nothing minted.
        allowedEdgeChildOrg(List.of("flights:read"), 0, "globex");
        String subject = tf.subjectTokenWithOrg("user:alice", "flights:read",
                ExchangeTestFactory.act(PARENT_SPIFFE, null), List.of("acme"));
        TokenExchangeContext c = ctx(subject, "flights:read", null, ACCESS, "actor-svid", JWT);

        assertError(c, actorVerifier(CHILD_SPIFFE, "child-1"), OAuthErrorException.INVALID_GRANT);
    }

    @Test
    void orgLessSubject_orgChild_passesAndStampsChildOrg() throws Exception {
        // An org-less subject token (realm-admin/global chain) carries no org
        // constraint, so it passes; the exchanged token is stamped from the
        // child's org.
        allowedEdgeChildOrg(List.of("flights:read"), 0, "acme");
        String subject = tf.subjectToken("user:alice", "flights:read",
                ExchangeTestFactory.act(PARENT_SPIFFE, null));
        TokenExchangeContext c = ctx(subject, "flights:read", null, ACCESS, "actor-svid", JWT);

        Response resp = provider(actorVerifier(CHILD_SPIFFE, "child-1")).exchange(c);
        assertEquals(200, resp.getStatus());
        assertEquals(List.of("acme"), minted.getOtherClaims().get("organization"));
    }

    // ── policy rejections ───────────────────────────────────────────────────────

    @Test
    void deniedEdge_isInvalidGrant() throws Exception {
        when(registry.getAgent("parent-1"))
                .thenReturn(new AgentRecord("parent-1", "active", "alice", "travel-planner", null));
        when(registry.getAgent("child-1"))
                .thenReturn(new AgentRecord("child-1", "active", "alice", "flight-search", null));
        when(registry.getDelegationPolicy("travel-planner", "flight-search"))
                .thenReturn(new DelegationPolicy(false, List.of(), 0));
        String subject = tf.subjectToken("user:alice", "flights:read",
                ExchangeTestFactory.act(PARENT_SPIFFE, null));
        TokenExchangeContext c = ctx(subject, "flights:read", null, ACCESS, "actor-svid", JWT);

        assertError(c, actorVerifier(CHILD_SPIFFE, "child-1"), OAuthErrorException.INVALID_GRANT);
    }

    @Test
    void scopeCeilingViolation_isInvalidGrant() throws Exception {
        // Edge allowed but the ceiling does NOT include the requested scope,
        // even though it IS present in the subject token.
        allowedEdge(List.of("hotels:read"), 0);
        String subject = tf.subjectToken("user:alice", "flights:read",
                ExchangeTestFactory.act(PARENT_SPIFFE, null));
        TokenExchangeContext c = ctx(subject, "flights:read", null, ACCESS, "actor-svid", JWT);

        assertError(c, actorVerifier(CHILD_SPIFFE, "child-1"), OAuthErrorException.INVALID_GRANT);
    }

    @Test
    void depthAtLimit_allowed() throws Exception {
        // existing chain depth 1 (parent) + this actor = 2; maxDepth 2 ⇒ allowed.
        allowedEdge(List.of("flights:read"), 2);
        String subject = tf.subjectToken("user:alice", "flights:read",
                ExchangeTestFactory.act(PARENT_SPIFFE, null));
        TokenExchangeContext c = ctx(subject, "flights:read", null, ACCESS, "actor-svid", JWT);

        Response resp = provider(actorVerifier(CHILD_SPIFFE, "child-1")).exchange(c);
        assertEquals(200, resp.getStatus());
    }

    @Test
    void depthOverLimit_isInvalidGrant() throws Exception {
        // chain: parent wraps grandparent ⇒ depth 2; + this actor = 3; maxDepth 2.
        allowedEdge(List.of("flights:read"), 2);
        when(registry.getAgent("gp-1"))
                .thenReturn(new AgentRecord("gp-1", "active", "alice", "orchestrator", null));
        Map<String, Object> chain = ExchangeTestFactory.act(PARENT_SPIFFE,
                ExchangeTestFactory.act(GRANDPARENT_SPIFFE, null));
        String subject = tf.subjectToken("user:alice", "flights:read", chain);
        TokenExchangeContext c = ctx(subject, "flights:read", null, ACCESS, "actor-svid", JWT);

        assertError(c, actorVerifier(CHILD_SPIFFE, "child-1"), OAuthErrorException.INVALID_GRANT);
    }

    // ── chain-revocation at each position ─────────────────────────────────────────

    @Test
    void revokedChild_isInvalidGrant() throws Exception {
        allowedEdge(List.of("flights:read"), 0);
        // Override the child to revoked.
        when(registry.getAgent("child-1"))
                .thenReturn(new AgentRecord("child-1", "revoked", "alice", "flight-search", null));
        String subject = tf.subjectToken("user:alice", "flights:read",
                ExchangeTestFactory.act(PARENT_SPIFFE, null));
        TokenExchangeContext c = ctx(subject, "flights:read", null, ACCESS, "actor-svid", JWT);

        assertError(c, actorVerifier(CHILD_SPIFFE, "child-1"), OAuthErrorException.INVALID_GRANT);
    }

    @Test
    void revokedImmediateParent_isInvalidGrant() throws Exception {
        allowedEdge(List.of("flights:read"), 0);
        when(registry.getAgent("parent-1"))
                .thenReturn(new AgentRecord("parent-1", "revoked", "alice", "travel-planner", null));
        // Re-prime the policy since the parentType is still travel-planner (agentType
        // resolves from the record regardless of status), so it reaches revocation.
        when(registry.getDelegationPolicy("travel-planner", "flight-search"))
                .thenReturn(new DelegationPolicy(true, List.of("flights:read"), 0));
        String subject = tf.subjectToken("user:alice", "flights:read",
                ExchangeTestFactory.act(PARENT_SPIFFE, null));
        TokenExchangeContext c = ctx(subject, "flights:read", null, ACCESS, "actor-svid", JWT);

        assertError(c, actorVerifier(CHILD_SPIFFE, "child-1"), OAuthErrorException.INVALID_GRANT);
    }

    @Test
    void revokedDeeperAncestor_isInvalidGrant() throws Exception {
        allowedEdge(List.of("flights:read"), 0);
        // grandparent present but revoked; it's an ANCESTOR, not the immediate parent.
        when(registry.getAgent("gp-1"))
                .thenReturn(new AgentRecord("gp-1", "revoked", "alice", "orchestrator", null));
        Map<String, Object> chain = ExchangeTestFactory.act(PARENT_SPIFFE,
                ExchangeTestFactory.act(GRANDPARENT_SPIFFE, null));
        String subject = tf.subjectToken("user:alice", "flights:read", chain);
        TokenExchangeContext c = ctx(subject, "flights:read", null, ACCESS, "actor-svid", JWT);

        assertError(c, actorVerifier(CHILD_SPIFFE, "child-1"), OAuthErrorException.INVALID_GRANT);
    }

    // ── act / issuer / scope edge cases ───────────────────────────────────────────

    @Test
    void absentAct_cannotResolveParties_isInvalidGrant() throws Exception {
        // No act claim at all ⇒ no parent to resolve.
        String subject = tf.subjectToken("user:alice", "flights:read", null);
        TokenExchangeContext c = ctx(subject, "flights:read", null, ACCESS, "actor-svid", JWT);

        assertError(c, actorVerifier(CHILD_SPIFFE, "child-1"), OAuthErrorException.INVALID_GRANT);
    }

    @Test
    void malformedAct_noSub_cannotResolveParties() throws Exception {
        // act is an object but has no top-level "sub".
        Map<String, Object> bad = new java.util.LinkedHashMap<>();
        bad.put("notsub", "x");
        String subject = tf.subjectToken("user:alice", "flights:read", bad);
        TokenExchangeContext c = ctx(subject, "flights:read", null, ACCESS, "actor-svid", JWT);

        assertError(c, actorVerifier(CHILD_SPIFFE, "child-1"), OAuthErrorException.INVALID_GRANT);
    }

    @Test
    void foreignIssuerSubjectToken_isInvalidRequest() throws Exception {
        // Signed by a DIFFERENT key AND carrying a foreign issuer: the realm-key
        // lookup misses (unknown kid) so verification fails.
        ExchangeTestFactory foreign = ExchangeTestFactory.foreign();
        String subject = foreign.subjectToken("user:alice", "flights:read",
                ExchangeTestFactory.act(PARENT_SPIFFE, null),
                "https://evil.example/realms/spawnly",
                new java.util.Date(System.currentTimeMillis() + 300_000));
        TokenExchangeContext c = ctx(subject, "flights:read", null, ACCESS, "actor-svid", JWT);

        assertError(c, actorVerifier(CHILD_SPIFFE, "child-1"), OAuthErrorException.INVALID_REQUEST);
    }

    @Test
    void selfIssuerButWrongSigningKey_isInvalidRequest() throws Exception {
        // Correct issuer string but signed by a foreign key ⇒ signature fails
        // (kid unknown to the realm key lookup).
        ExchangeTestFactory foreign = ExchangeTestFactory.foreign();
        String subject = foreign.subjectToken("user:alice", "flights:read",
                ExchangeTestFactory.act(PARENT_SPIFFE, null));
        TokenExchangeContext c = ctx(subject, "flights:read", null, ACCESS, "actor-svid", JWT);

        assertError(c, actorVerifier(CHILD_SPIFFE, "child-1"), OAuthErrorException.INVALID_REQUEST);
    }

    @Test
    void missingScope_isInvalidScope() throws Exception {
        allowedEdge(List.of("flights:read"), 0);
        String subject = tf.subjectToken("user:alice", "flights:read",
                ExchangeTestFactory.act(PARENT_SPIFFE, null));
        TokenExchangeContext c = ctx(subject, null, null, ACCESS, "actor-svid", JWT);

        assertError(c, actorVerifier(CHILD_SPIFFE, "child-1"), OAuthErrorException.INVALID_SCOPE);
    }

    @Test
    void requestedScopeNotInSubject_isInvalidScope() throws Exception {
        allowedEdge(List.of("flights:read", "hotels:read"), 0);
        // subject only carries flights:read; request hotels:read too.
        String subject = tf.subjectToken("user:alice", "flights:read",
                ExchangeTestFactory.act(PARENT_SPIFFE, null));
        TokenExchangeContext c = ctx(subject, "flights:read hotels:read", null, ACCESS, "actor-svid", JWT);

        assertError(c, actorVerifier(CHILD_SPIFFE, "child-1"), OAuthErrorException.INVALID_SCOPE);
    }

    // ── token typing / presence ───────────────────────────────────────────────────

    @Test
    void missingActorToken_isInvalidRequest() throws Exception {
        String subject = tf.subjectToken("user:alice", "flights:read",
                ExchangeTestFactory.act(PARENT_SPIFFE, null));
        TokenExchangeContext c = ctx(subject, "flights:read", null, ACCESS, null, JWT);

        assertError(c, actorVerifier(CHILD_SPIFFE, "child-1"), OAuthErrorException.INVALID_REQUEST);
    }

    @Test
    void wrongSubjectTokenType_isInvalidRequest() throws Exception {
        String subject = tf.subjectToken("user:alice", "flights:read",
                ExchangeTestFactory.act(PARENT_SPIFFE, null));
        TokenExchangeContext c = ctx(subject, "flights:read", null,
                "urn:ietf:params:oauth:token-type:saml2", "actor-svid", JWT);

        assertError(c, actorVerifier(CHILD_SPIFFE, "child-1"), OAuthErrorException.INVALID_REQUEST);
    }

    @Test
    void invalidActorToken_isInvalidRequest() throws Exception {
        String subject = tf.subjectToken("user:alice", "flights:read",
                ExchangeTestFactory.act(PARENT_SPIFFE, null));
        TokenExchangeContext c = ctx(subject, "flights:read", null, ACCESS, "bad-svid", JWT);
        AgentCredentialVerifier v = mock(AgentCredentialVerifier.class);
        when(v.verify(anyString())).thenReturn(null); // rejected credential

        assertError(c, v, OAuthErrorException.INVALID_REQUEST);
    }

    // ── supports() routing ─────────────────────────────────────────────────────────

    @Test
    void supports_trueForSelfIssuedWithActor() throws Exception {
        String subject = tf.subjectToken("user:alice", "flights:read",
                ExchangeTestFactory.act(PARENT_SPIFFE, null));
        TokenExchangeContext c = ctx(subject, "flights:read", null, ACCESS, "actor-svid", JWT);
        assertTrue(provider(actorVerifier(CHILD_SPIFFE, "child-1")).supports(c));
    }

    @Test
    void supports_falseWhenNoActorToken() throws Exception {
        String subject = tf.subjectToken("user:alice", "flights:read", null);
        TokenExchangeContext c = ctx(subject, "flights:read", null, ACCESS, null, JWT);
        assertFalse(provider(actorVerifier(CHILD_SPIFFE, "child-1")).supports(c));
    }

    @Test
    void supports_falseForForeignIssuer() throws Exception {
        ExchangeTestFactory foreign = ExchangeTestFactory.foreign();
        String subject = foreign.subjectToken("user:alice", "flights:read",
                ExchangeTestFactory.act(PARENT_SPIFFE, null),
                "https://other.example/realms/x",
                new java.util.Date(System.currentTimeMillis() + 300_000));
        TokenExchangeContext c = ctx(subject, "flights:read", null, ACCESS, "actor-svid", JWT);
        assertFalse(provider(actorVerifier(CHILD_SPIFFE, "child-1")).supports(c));
    }

    // ── helper ──────────────────────────────────────────────────────────────────────

    private void assertError(TokenExchangeContext c, AgentCredentialVerifier v, String expectedError) {
        WebApplicationException ex = assertThrows(WebApplicationException.class,
                () -> provider(v).exchange(c));
        // The error code is carried in the CorsErrorResponseException; assert we
        // got the right OAuth error class of failure.
        assertEquals(expectedError, errorOf(ex));
        // Nothing was minted on a rejection.
        assertNull(minted);
    }

    /** Reads the OAuth error code the CorsErrorResponseException was built with. */
    private static String errorOf(WebApplicationException ex) {
        // CorsErrorResponseException stores the error in its message-less form;
        // fall back to reflecting the entity if present. We compare against the
        // known error strings by inspecting the exception's toString/detail.
        Throwable t = ex;
        String msg = String.valueOf(t.getMessage());
        // CorsErrorResponseException(cors, error, detail, status): getMessage() is
        // the error code.
        return msg;
    }
}
