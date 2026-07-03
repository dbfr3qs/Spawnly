package run.spawnly.keycloak.exchange;

import jakarta.ws.rs.core.MultivaluedMap;
import jakarta.ws.rs.core.Response;
import org.jboss.logging.Logger;
import org.keycloak.OAuth2Constants;
import org.keycloak.OAuthErrorException;
import org.keycloak.TokenVerifier;
import org.keycloak.crypto.KeyUse;
import org.keycloak.crypto.KeyWrapper;
import org.keycloak.jose.jws.JWSInput;
import org.keycloak.models.ClientModel;
import org.keycloak.models.KeycloakSession;
import org.keycloak.models.RealmModel;
import org.keycloak.protocol.oidc.TokenExchangeContext;
import org.keycloak.protocol.oidc.TokenExchangeProvider;
import org.keycloak.representations.AccessToken;
import org.keycloak.representations.AccessTokenResponse;
import org.keycloak.representations.JsonWebToken;
import org.keycloak.services.CorsErrorResponseException;
import org.keycloak.services.Urls;
import org.keycloak.services.cors.Cors;
import org.keycloak.util.JsonSerialization;

import run.spawnly.keycloak.attestor.AgentCredentialVerifier;
import run.spawnly.keycloak.attestor.AgentIdentity;
import run.spawnly.keycloak.registry.AgentRecord;
import run.spawnly.keycloak.registry.AgentRegistryClient;
import run.spawnly.keycloak.registry.DelegationPolicy;

import java.security.PublicKey;
import java.util.Arrays;
import java.util.HashSet;
import java.util.LinkedHashMap;
import java.util.LinkedHashSet;
import java.util.List;
import java.util.Map;
import java.util.Set;

/**
 * RFC 8693 OAuth2 Token Exchange for Spawnly delegation — the Keycloak port of
 * identityserver/TokenExchangeGrantValidator.cs. The delegation semantics are
 * FROZEN and reproduced here exactly.
 *
 * <p><b>How it plugs into Keycloak.</b> Keycloak's built-in
 * {@code TokenExchangeGrantType} collects every {@link TokenExchangeProvider}
 * factory, sorts them by {@code order()} descending, and dispatches the request
 * to the FIRST provider whose {@link #supports(TokenExchangeContext)} returns
 * true. This provider's factory registers with a higher {@code order()} than the
 * built-in Standard (10) / V1 providers, so it gets first refusal; when the
 * exchange is not a Spawnly delegation ({@link #supports} returns false) the
 * built-in providers still handle it. So the {@code token-exchange} feature flag
 * (which only gates the legacy v1 grant) can stay enabled — this provider OWNS
 * the URN for Spawnly delegation exchanges by priority, no collision.
 *
 * <p><b>Flow (identical to the C#).</b> An agent (the actor, authenticated by
 * its SPIRE JWT-SVID in {@code actor_token}) exchanges a token THIS realm issued
 * ({@code subject_token}) for a new, optionally scope-narrowed / audience-
 * retargeted token:
 * <ul>
 *   <li>{@code sub} = subject_token.sub (the user, carried unchanged);</li>
 *   <li>{@code act} = {"sub":"&lt;actor spiffe&gt;","act":&lt;subject act verbatim&gt;}
 *       (the new actor wraps the prior chain);</li>
 *   <li>{@code scope} = the requested (validated) scopes;</li>
 *   <li>{@code aud} from the granted scopes' audience mappings; {@code
 *       audience=delegation} ⇒ no resource aud + {@code token_use=delegation}.</li>
 * </ul>
 *
 * <p><b>Enforcement, in order (all frozen):</b>
 * <ol>
 *   <li>token typing/presence: subject_token + actor_token required,
 *       access_token/jwt types — else invalid_request;</li>
 *   <li>actor_token verified via the attestor seam → its subject is the new
 *       actor, its agentId is the child;</li>
 *   <li>subject_token self-issued by THIS realm (realm signature + issuer, NO
 *       audience check — a delegation token has no resource aud);</li>
 *   <li>scope required, and requested ⊆ subject_token.scope — else
 *       invalid_scope (Keycloak issues EXACTLY the requested scopes here, it
 *       can't widen to the client's full allowed set);</li>
 *   <li>registry delegation policy: parent = outermost act.sub, child = actor's
 *       agentId; edge allowed; requested ⊆ grantableScopes ceiling; new depth ≤
 *       maxDepth (when maxDepth&gt;0) — else invalid_grant;</li>
 *   <li>chain-revocation: {child} ∪ every actor in the subject_token's act chain
 *       must be registry status {@code active} — else invalid_grant.</li>
 * </ol>
 *
 * <p>SECURITY (frozen): scope-subset can't be bypassed by omitting {@code scope}
 * (empty scope is rejected); {@code sub}/{@code role} are NEVER derived from the
 * actor ({@code sub} is copied from the subject_token verbatim, {@code role} is
 * never emitted); chain-revocation covers ALL ancestors, not just the immediate
 * parent; a foreign-issuer subject_token is rejected at step 3; the emitted
 * delegation token carries {@code token_use=delegation} so resource servers
 * reject it.
 *
 * <p>DEPENDENCY DIRECTION: depends only on the registry's neutral HTTP contract
 * ({@link AgentRegistryClient}) and the attestation credential format
 * ({@link AgentCredentialVerifier}) + Keycloak's SPI — never the reverse.
 */
public class SpawnlyTokenExchangeProvider implements TokenExchangeProvider {

    private static final Logger log = Logger.getLogger(SpawnlyTokenExchangeProvider.class);

    /** Sentinel audience: "not for any resource server — only re-exchangeable". */
    public static final String DELEGATION_AUDIENCE = "delegation";

    private static final String ACCESS_TOKEN_TYPE = "urn:ietf:params:oauth:token-type:access_token";
    private static final String JWT_TOKEN_TYPE = "urn:ietf:params:oauth:token-type:jwt";
    private static final String ACT_CLAIM = "act";
    private static final String TOKEN_USE_CLAIM = "token_use";

    // Access-token lifetime for the exchanged token, in seconds. Matches the
    // 120s the C#/Config.cs machine clients use; a delegated token is short-lived
    // and request-specific so a fixed conservative lifetime is correct.
    private static final long EXCHANGED_TOKEN_LIFESPAN_SECONDS = 120L;

    private final KeycloakSession session;
    private final AgentCredentialVerifier verifier;
    private final AgentRegistryClient registry;
    // scope -> audience (resource) name, mirroring deploy/keycloak/scopes.tf's
    // scope_audience map (Config.cs ApiResources). A delegation token requests
    // audience=delegation and carries none of these.
    private final Map<String, String> scopeAudience;

    public SpawnlyTokenExchangeProvider(KeycloakSession session, AgentCredentialVerifier verifier,
                                        AgentRegistryClient registry, Map<String, String> scopeAudience) {
        this.session = session;
        this.verifier = verifier;
        this.registry = registry;
        this.scopeAudience = scopeAudience;
    }

    @Override
    public int getVersion() {
        return 1;
    }

    /**
     * True only for a Spawnly delegation exchange: an {@code actor_token} is
     * present AND the {@code subject_token} is a token this realm issued (its
     * issuer equals the realm issuer). Everything else (no actor_token, or a
     * foreign/broker subject issuer) is left to the built-in providers, so this
     * provider owns ONLY the Spawnly delegation URN by priority.
     *
     * <p>Reading the subject_token issuer here is unauthenticated (signature not
     * yet checked) and used ONLY for routing; {@link #exchange} re-validates the
     * signature + issuer authoritatively before trusting any claim.
     */
    @Override
    public boolean supports(TokenExchangeContext context) {
        TokenExchangeContext.Params params = context.getParams();
        String actorToken = params.getActorToken();
        String subjectToken = params.getSubjectToken();
        if (actorToken == null || actorToken.isEmpty()
                || subjectToken == null || subjectToken.isEmpty()) {
            return false;
        }
        String realmIssuer = realmIssuer(context);
        String subjectIssuer = unverifiedIssuer(subjectToken);
        return realmIssuer != null && realmIssuer.equals(subjectIssuer);
    }

    @Override
    public Response exchange(TokenExchangeContext context) {
        TokenExchangeContext.Params params = context.getParams();
        Cors cors = context.getCors();

        String subjectToken = params.getSubjectToken();
        String subjectTokenType = params.getSubjectTokenType();
        String actorToken = params.getActorToken();
        String actorTokenType = params.getActorTokenType();
        String requestedScope = params.getScope();
        String requestedAudience = firstAudience(context);

        // ── 1. Token typing & presence ────────────────────────────────────────
        if (subjectToken == null || subjectToken.isEmpty()) {
            throw invalidRequest(cors, "subject_token is required");
        }
        if (!isAccessTokenType(subjectTokenType)) {
            throw invalidRequest(cors, "unsupported subject_token_type");
        }
        if (actorToken == null || actorToken.isEmpty()) {
            throw invalidRequest(cors, "actor_token is required");
        }
        if (!isAccessTokenType(actorTokenType)) {
            throw invalidRequest(cors, "unsupported actor_token_type");
        }

        // ── 2. actor_token: a valid attestation credential (the new actor) ─────
        AgentIdentity actorIdentity;
        try {
            actorIdentity = verifier.verify(actorToken);
        } catch (RuntimeException e) {
            log.debugf(e, "actor_token attestation verify threw");
            actorIdentity = null;
        }
        if (actorIdentity == null) {
            throw invalidRequest(cors, "invalid actor_token (credential validation failed)");
        }
        String actorSpiffe = actorIdentity.subject();
        String childAgentId = actorIdentity.agentId();

        // ── 3. subject_token: self-issued by THIS realm (sig + issuer) ─────────
        //      NO audience check — a delegation token has no resource audience.
        AccessToken subject = validateSubjectToken(context, subjectToken);
        if (subject == null) {
            throw invalidRequest(cors, "invalid subject_token");
        }
        String sub = subject.getSubject();
        if (sub == null || sub.isEmpty()) {
            throw invalidRequest(cors, "subject_token has no sub");
        }

        // ── 4. Scope subset: requested ⊆ subject_token.scope ───────────────────
        //      scope is REQUIRED (omitting it would otherwise let the token widen
        //      to the client's full allowed set, exceeding the subject grant).
        Set<String> subjectScopes = splitScopes(subject.getScope());
        List<String> requestedScopes = Arrays.stream(
                        (requestedScope == null ? "" : requestedScope).split(" "))
                .filter(s -> !s.isEmpty())
                .distinct()
                .toList();
        if (requestedScopes.isEmpty()) {
            throw invalidScope(cors, "scope is required for token exchange");
        }
        List<String> disallowed = requestedScopes.stream()
                .filter(s -> !subjectScopes.contains(s))
                .toList();
        if (!disallowed.isEmpty()) {
            throw invalidScope(cors,
                    "requested scope(s) not present in subject_token: " + String.join(" ", disallowed));
        }

        // ── 5. Delegation-policy enforcement ───────────────────────────────────
        //      parent = OUTERMOST act.sub of the subject_token (its agentId = last
        //      SPIFFE path segment); child = the actor doing this exchange.
        String parentSpiffe = outermostActSub(subject);
        if (parentSpiffe == null || parentSpiffe.isEmpty()) {
            throw invalidGrant(cors, "cannot resolve delegation parties");
        }
        String parentAgentId = lastPathSegment(parentSpiffe);
        if (childAgentId == null || childAgentId.isEmpty()
                || parentAgentId == null || parentAgentId.isEmpty()) {
            throw invalidGrant(cors, "cannot resolve delegation parties");
        }

        AgentRecord childAgent = registry.getAgent(childAgentId);
        AgentRecord parentAgent = registry.getAgent(parentAgentId);
        String childType = childAgent == null ? null : childAgent.agentType();
        String parentType = parentAgent == null ? null : parentAgent.agentType();
        if (childType == null || childType.isEmpty() || parentType == null || parentType.isEmpty()) {
            throw invalidGrant(cors, "cannot resolve delegation parties");
        }

        DelegationPolicy policy = registry.getDelegationPolicy(parentType, childType);
        if (policy == null || !policy.allowed()) {
            throw invalidGrant(cors, "delegation not permitted: " + parentType + " -> " + childType);
        }

        // Scope ceiling: every requested scope must be grantable across this edge
        // (in addition to requested ⊆ subject_token.scope above).
        Set<String> grantable = policy.grantableScopes() == null
                ? Set.of() : new HashSet<>(policy.grantableScopes());
        for (String s : requestedScopes) {
            if (!grantable.contains(s)) {
                throw invalidGrant(cors,
                        "scope '" + s + "' exceeds delegation ceiling for " + parentType + " -> " + childType);
            }
        }

        // Chain depth: new chain = existing act-chain depth + this actor.
        int newDepth = countActChainDepth(subject) + 1;
        if (policy.maxDepth() > 0 && newDepth > policy.maxDepth()) {
            throw invalidGrant(cors,
                    "delegation chain depth " + newDepth + " exceeds max " + policy.maxDepth());
        }

        // ── 6. Chain-revocation: {child} ∪ act chain must all be active ────────
        Set<String> chainAgentIds = new LinkedHashSet<>();
        chainAgentIds.add(childAgentId);
        chainAgentIds.addAll(allActChainAgentIds(subject));
        for (String id : chainAgentIds) {
            AgentRecord agent = registry.getAgent(id);
            if (agent == null || !"active".equals(agent.status())) {
                throw invalidGrant(cors, "delegation chain member " + id + " is not active (status: "
                        + (agent == null ? "unknown" : agent.status()) + ")");
            }
        }

        // ── 7. Emit the new (signed) token ─────────────────────────────────────
        boolean delegation = DELEGATION_AUDIENCE.equals(requestedAudience);
        String signed = mintToken(context, sub, actorSpiffe, subject, requestedScopes, delegation);

        AccessTokenResponse resp = new AccessTokenResponse();
        resp.setToken(signed);
        resp.setTokenType("Bearer");
        resp.setExpiresIn(EXCHANGED_TOKEN_LIFESPAN_SECONDS);
        resp.setScope(String.join(" ", requestedScopes));
        // RFC 8693 issued_token_type of the returned token.
        resp.setOtherClaims(OAuth2Constants.ISSUED_TOKEN_TYPE, ACCESS_TOKEN_TYPE);

        try {
            return cors.add(Response.ok(JsonSerialization.writeValueAsBytes(resp))
                    .type(jakarta.ws.rs.core.MediaType.APPLICATION_JSON_TYPE));
        } catch (Exception e) {
            log.error("failed to serialize token-exchange response", e);
            throw invalidRequest(cors, "failed to build token response");
        }
    }

    @Override
    public void close() {
        // Verifier + registry client are shared, factory-owned, and stateless;
        // nothing per-request to release.
    }

    // ── Token minting ──────────────────────────────────────────────────────────

    /**
     * Builds and signs the exchanged access token with the realm's active key.
     * {@code sub} is carried from the subject_token verbatim; {@code act} wraps
     * the new actor around the prior chain; scope is the requested (validated)
     * set; audience is derived from the granted scopes' audience mapping unless
     * this is a delegation-only token (then NO resource aud + token_use).
     */
    private String mintToken(TokenExchangeContext context, String sub, String actorSpiffe,
                             AccessToken subject, List<String> scopes, boolean delegation) {
        long now = System.currentTimeMillis() / 1000L;
        AccessToken out = new AccessToken();
        out.id(org.keycloak.models.utils.KeycloakModelUtils.generateId());
        out.issuer(realmIssuer(context));
        out.issuedFor(context.getClient().getClientId());
        out.setSubject(sub);
        out.iat(now);
        out.exp(now + EXCHANGED_TOKEN_LIFESPAN_SECONDS);
        out.type("Bearer");
        out.setScope(String.join(" ", scopes));

        // act = { "sub": "<actor spiffe>", "act": <subject act verbatim> }.
        out.setOtherClaims(ACT_CLAIM, buildActClaim(actorSpiffe, subject));

        if (delegation) {
            // Delegation-only token: NO resource audience, token_use=delegation.
            out.setOtherClaims(TOKEN_USE_CLAIM, DELEGATION_AUDIENCE);
        } else {
            // Resource audience from the granted scopes' audience mapping,
            // reproducing scopes.tf's per-scope audience mappers.
            LinkedHashSet<String> auds = new LinkedHashSet<>();
            for (String s : scopes) {
                String aud = scopeAudience.get(s);
                if (aud != null) {
                    auds.add(aud);
                }
            }
            if (!auds.isEmpty()) {
                out.audience(auds.toArray(new String[0]));
            }
        }

        // Sign with the realm's active signing key via the token SPI.
        return session.tokens().encode(out);
    }

    /**
     * Builds the new act claim as a nested Map (Keycloak serializes an
     * otherClaims Map value as a JSON object, reproducing the C#
     * JsonClaimValueTypes.Json shape):
     * {@code { "sub": "<actor>", "act": <previous act> }}. The previous act (if
     * any) is embedded verbatim as a nested object, preserving the full chain.
     */
    private static Map<String, Object> buildActClaim(String actorSpiffe, AccessToken subject) {
        Map<String, Object> act = new LinkedHashMap<>();
        act.put("sub", actorSpiffe);
        Object prev = subject.getOtherClaims().get(ACT_CLAIM);
        if (prev != null) {
            act.put(ACT_CLAIM, prev);
        }
        return act;
    }

    // ── subject_token validation (self-issued by this realm) ─────────────────────

    /**
     * Validates the subject_token's signature against the realm's signing keys
     * and confirms the issuer matches the realm issuer. Lifetime (exp/nbf/iat) is
     * checked. Audience is NOT validated — a delegation token has no resource
     * audience. Returns the parsed token on success, else null (never throws).
     */
    private AccessToken validateSubjectToken(TokenExchangeContext context, String token) {
        try {
            String kid = new JWSInput(token).getHeader().getKeyId();
            String alg = new JWSInput(token).getHeader().getRawAlgorithm();
            RealmModel realm = context.getRealm();

            KeyWrapper key = (kid != null)
                    ? session.keys().getKey(realm, kid, KeyUse.SIG, alg)
                    : session.keys().getActiveKey(realm, KeyUse.SIG, alg);
            if (key == null || !(key.getPublicKey() instanceof PublicKey)) {
                log.debugf("no realm SIG key for kid=%s alg=%s", kid, alg);
                return null;
            }

            TokenVerifier<AccessToken> verifier = TokenVerifier.create(token, AccessToken.class)
                    .realmUrl(realmIssuer(context))
                    .checkRealmUrl(true)   // issuer must equal the realm issuer
                    .checkActive(true)     // exp/nbf/iat
                    .checkTokenType(false) // exchanged tokens carry type=Bearer, not KC internal
                    .publicKey((PublicKey) key.getPublicKey());
            verifier.verify(); // throws on bad signature / wrong issuer / expired
            return verifier.getToken();
        } catch (Exception e) {
            log.debugf(e, "subject_token validation failed");
            return null;
        }
    }

    // ── act-chain helpers (ported verbatim from the C#) ──────────────────────────

    /**
     * The outermost actor's "sub" from the subject_token's act claim — the
     * immediate delegator. act is a nested object { "sub": ..., "act": {...} };
     * the top-level "sub" is the most recent actor. Null if there is no act, it
     * is not an object, or it has no top-level string "sub".
     */
    @SuppressWarnings("unchecked")
    static String outermostActSub(AccessToken subject) {
        Object act = subject.getOtherClaims().get(ACT_CLAIM);
        if (act instanceof Map<?, ?> m) {
            Object s = ((Map<String, Object>) m).get("sub");
            if (s instanceof String str && !str.isEmpty()) {
                return str;
            }
        }
        return null;
    }

    /**
     * The agentId of every actor named in the subject_token's act chain: walk the
     * nested act objects, mapping each level's "sub" (a SPIFFE URI) to its
     * agentId via {@link #lastPathSegment}. Levels without a usable "sub" are
     * skipped. No act ⇒ empty.
     */
    @SuppressWarnings("unchecked")
    static List<String> allActChainAgentIds(AccessToken subject) {
        List<String> ids = new java.util.ArrayList<>();
        Object node = subject.getOtherClaims().get(ACT_CLAIM);
        while (node instanceof Map<?, ?> m) {
            Map<String, Object> map = (Map<String, Object>) m;
            Object s = map.get("sub");
            if (s instanceof String str) {
                String id = lastPathSegment(str);
                if (id != null && !id.isEmpty()) {
                    ids.add(id);
                }
            }
            node = map.get(ACT_CLAIM);
        }
        return ids;
    }

    /**
     * Counts actors already in the subject_token's act chain: the outermost act
     * counts as 1, plus 1 per nested "act". No act ⇒ depth 0. Each structural
     * level is counted once (tracks the chain length built by buildActClaim),
     * matching the C# CountActChainDepth.
     */
    @SuppressWarnings("unchecked")
    static int countActChainDepth(AccessToken subject) {
        int depth = 0;
        Object node = subject.getOtherClaims().get(ACT_CLAIM);
        while (node instanceof Map<?, ?> m) {
            depth++;
            node = ((Map<String, Object>) m).get(ACT_CLAIM);
        }
        return depth;
    }

    /**
     * agentId = last path segment of a SPIFFE URI
     * (spiffe://cluster.local/agent/&lt;tenant&gt;/&lt;user&gt;/&lt;type&gt;/&lt;agentId&gt;).
     */
    static String lastPathSegment(String spiffe) {
        if (spiffe == null || spiffe.isEmpty()) {
            return null;
        }
        String[] segs = spiffe.split("/");
        for (int i = segs.length - 1; i >= 0; i--) {
            if (!segs[i].isEmpty()) {
                return segs[i];
            }
        }
        return null;
    }

    // ── small utilities ──────────────────────────────────────────────────────────

    private static boolean isAccessTokenType(String type) {
        return ACCESS_TOKEN_TYPE.equals(type) || JWT_TOKEN_TYPE.equals(type);
    }

    private static Set<String> splitScopes(String scope) {
        Set<String> out = new HashSet<>();
        if (scope != null) {
            for (String s : scope.split(" ")) {
                if (!s.isEmpty()) {
                    out.add(s);
                }
            }
        }
        return out;
    }

    /** The realm issuer URL: {@code <server>/realms/<realm>}. */
    private static String realmIssuer(TokenExchangeContext context) {
        return Urls.realmIssuer(context.getSession().getContext().getUri().getBaseUri(),
                context.getRealm().getName());
    }

    /** The first requested audience value, or null. */
    private static String firstAudience(TokenExchangeContext context) {
        List<String> aud = context.getParams().getAudience();
        if (aud == null || aud.isEmpty()) {
            // Fall back to the raw form param for a single audience value.
            MultivaluedMap<String, String> form = context.getFormParams();
            return form == null ? null : form.getFirst(OAuth2Constants.AUDIENCE);
        }
        return aud.get(0);
    }

    /**
     * Reads the {@code iss} claim of a JWT WITHOUT verifying its signature — used
     * only by {@link #supports} for routing. The authoritative signature+issuer
     * check happens in {@link #validateSubjectToken}.
     */
    private static String unverifiedIssuer(String token) {
        try {
            JsonWebToken jwt = TokenVerifier.create(token, JsonWebToken.class).getToken();
            return jwt.getIssuer();
        } catch (Exception e) {
            return null;
        }
    }

    private static CorsErrorResponseException invalidRequest(Cors cors, String detail) {
        log.debugf("token-exchange invalid_request: %s", detail);
        return new CorsErrorResponseException(cors, OAuthErrorException.INVALID_REQUEST, detail,
                Response.Status.BAD_REQUEST);
    }

    private static CorsErrorResponseException invalidScope(Cors cors, String detail) {
        log.debugf("token-exchange invalid_scope: %s", detail);
        return new CorsErrorResponseException(cors, OAuthErrorException.INVALID_SCOPE, detail,
                Response.Status.BAD_REQUEST);
    }

    private static CorsErrorResponseException invalidGrant(Cors cors, String detail) {
        log.debugf("token-exchange invalid_grant: %s", detail);
        return new CorsErrorResponseException(cors, OAuthErrorException.INVALID_GRANT, detail,
                Response.Status.BAD_REQUEST);
    }
}
