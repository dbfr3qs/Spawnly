package run.spawnly.keycloak.attestor;

/**
 * Configuration for {@link StsWebCredentialVerifier}. Ported from the
 * {@code StsWebOptions} record in identityserver/StsWebCredentialVerifier.cs and
 * populated exactly as identityserver/Program.cs:42 does from the
 * {@code STSWEB_*} environment variables.
 *
 * @param jwksUrl        the account STS issuer's JWKS endpoint (required to
 *                       validate signatures); when empty it is derived from
 *                       {@code issuer + "/.well-known/jwks.json"} like Program.cs.
 * @param issuer         expected {@code iss} ({@code STSWEB_ISSUER}); empty = not
 *                       enforced.
 * @param audience       expected {@code aud} ({@code STSWEB_AUDIENCE},
 *                       default "spawnly"); empty = not enforced.
 * @param namespace      expected {@code principal_tags.kubernetes-namespace}
 *                       ({@code STSWEB_NAMESPACE}); empty = not enforced.
 * @param serviceAccount expected {@code principal_tags.kubernetes-service-account}
 *                       ({@code STSWEB_SERVICE_ACCOUNT}); empty = not enforced.
 * @param clusterArn     expected {@code principal_tags.eks-cluster-arn}
 *                       ({@code STSWEB_CLUSTER_ARN}); empty = not enforced (a
 *                       warning is logged — any EKS cluster in the account is
 *                       then accepted).
 * @param podSuffix      stripped from {@code kubernetes-pod-name} to derive the
 *                       agentId; empty defaults to "-pod".
 */
public record StsWebOptions(
        String jwksUrl,
        String issuer,
        String audience,
        String namespace,
        String serviceAccount,
        String clusterArn,
        String podSuffix) {
}
