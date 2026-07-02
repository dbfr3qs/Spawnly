package run.spawnly.keycloak.attestor;

import com.fasterxml.jackson.databind.JsonNode;
import org.jboss.logging.Logger;
import org.keycloak.util.JsonSerialization;

import javax.xml.parsers.DocumentBuilder;
import javax.xml.parsers.DocumentBuilderFactory;
import java.io.ByteArrayInputStream;
import java.net.URI;
import java.net.http.HttpClient;
import java.net.http.HttpRequest;
import java.net.http.HttpResponse;
import java.nio.charset.StandardCharsets;
import java.time.Duration;
import java.util.Base64;
import java.util.Iterator;
import java.util.Map;
import java.util.regex.Pattern;

import org.w3c.dom.Document;
import org.w3c.dom.Element;
import org.w3c.dom.Node;
import org.w3c.dom.NodeList;

/**
 * AWS STS attestor ({@code aws-sts}). Verifies a presigned
 * {@code GetCallerIdentity} credential by replaying it against AWS STS — AWS is
 * the attestor, since a valid SigV4 signature is required for STS to answer —
 * then derives {@link AgentIdentity#agentId()} from the assumed-role session
 * name (the operator sets the session name to the agentId).
 *
 * <p>Ported from identityserver/StsCredentialVerifier.cs and kept in lock step
 * with Go {@code internal/attestor/aws_verify.go} +
 * {@code internal/registrant/aws.go} so the registry and the IdP derive the
 * SAME {@code (agentId, subject, issuer)} for the same agent (the consistency
 * invariant): {@code agentId = <sessionName>}, {@code subject = <full ARN>},
 * {@code issuer = "aws-sts"}.
 *
 * <p>Implemented on the JDK HttpClient + the JDK XML parser (matching the way
 * the C# uses HttpClient + XDocument), so this stays a thin providers-only JAR
 * with NO shaded runtime dependency — the AWS SDK is deliberately NOT pulled
 * in. JSON decoding of the presigned envelope reuses Keycloak's own Jackson
 * ({@link JsonSerialization}), already on the runtime classpath.
 */
public class StsCredentialVerifier implements AgentCredentialVerifier {

    private static final Logger log = Logger.getLogger(StsCredentialVerifier.class);

    // Matches the AWS STS endpoints (global or regional). Identical to the C#
    // regex and Go stsHostPattern.
    private static final Pattern STS_HOST =
            Pattern.compile("^sts(\\.[a-z0-9-]+)?\\.amazonaws\\.com$");

    private static final String ASSUMED_ROLE_MARKER = ":assumed-role/";

    private final HttpClient http;

    public StsCredentialVerifier() {
        this(HttpClient.newBuilder()
                .connectTimeout(Duration.ofSeconds(10))
                .build());
    }

    /** For tests: inject a client (e.g. pointed at WireMock over plain http). */
    StsCredentialVerifier(HttpClient http) {
        this.http = http;
    }

    @Override
    public AgentIdentity verify(String credential) {
        if (credential == null || credential.isEmpty()) {
            return null;
        }
        PresignedRequest pr = decodePresigned(credential);
        if (pr == null || pr.url == null || pr.url.isEmpty() || pr.method == null || pr.method.isEmpty()) {
            return null;
        }
        // The verifier replays a caller-supplied URL, so it MUST confirm the URL
        // targets an STS endpoint over HTTPS and invokes GetCallerIdentity — else
        // a caller could point the control plane at an arbitrary URL (SSRF).
        if (!validateStsUrl(pr.url)) {
            return null;
        }

        String arn = replay(pr);
        if (arn == null || arn.isEmpty()) {
            return null;
        }
        String sessionName = sessionNameFromArn(arn);
        if (sessionName == null || sessionName.isEmpty()) {
            log.debug("aws-sts ARN has no session name");
            return null;
        }
        return new AgentIdentity(sessionName, arn, "aws-sts");
    }

    /**
     * Issues the presigned request and parses the attested ARN, or null on any
     * failure. Performs NO host validation — {@link #verify} is the only caller
     * and gates it behind {@link #validateStsUrl} first (mirrors Go
     * {@code replayGetCallerIdentity}). Package-private so a unit test can drive
     * the replay+parse path against a mock endpoint without tripping the SSRF
     * host guard, exactly as the Go test does.
     */
    String replay(PresignedRequest pr) {
        try {
            HttpRequest.Builder rb = HttpRequest.newBuilder(URI.create(pr.url))
                    .timeout(Duration.ofSeconds(10))
                    .method(pr.method, HttpRequest.BodyPublishers.noBody());
            if (pr.headers != null) {
                for (Map.Entry<String, String[]> e : pr.headers.entrySet()) {
                    if (e.getValue() == null) {
                        continue;
                    }
                    for (String v : e.getValue()) {
                        try {
                            rb.header(e.getKey(), v);
                        } catch (IllegalArgumentException restricted) {
                            // The JDK HttpClient forbids setting a few headers
                            // (Host, Content-Length, ...). SigV4 signs Host via
                            // the URL authority, and the JDK sets it from the
                            // URI, so skipping a restricted header is safe.
                            log.debugf("skipping restricted STS header %s", e.getKey());
                        }
                    }
                }
            }

            HttpResponse<String> resp = http.send(rb.build(), HttpResponse.BodyHandlers.ofString());
            if (resp.statusCode() != 200) {
                log.debugf("aws-sts GetCallerIdentity returned status %d", resp.statusCode());
                return null;
            }
            return parseArn(resp.body());
        } catch (Exception e) {
            // network / parse / bad response ⇒ rejected. Null is the single
            // "not authenticated" signal, matching the C# catch-all.
            log.debugf(e, "aws-sts replay failed");
            return null;
        }
    }

    static PresignedRequest decodePresigned(String credential) {
        try {
            byte[] json = Base64.getDecoder().decode(credential);
            JsonNode n = JsonSerialization.mapper.readTree(json);
            PresignedRequest pr = new PresignedRequest();
            pr.method = text(n, "method");
            pr.url = text(n, "url");
            JsonNode headers = n.get("headers");
            if (headers != null && headers.isObject()) {
                java.util.HashMap<String, String[]> map = new java.util.HashMap<>();
                Iterator<Map.Entry<String, JsonNode>> it = headers.fields();
                while (it.hasNext()) {
                    Map.Entry<String, JsonNode> e = it.next();
                    JsonNode arr = e.getValue();
                    if (arr != null && arr.isArray()) {
                        String[] vals = new String[arr.size()];
                        for (int i = 0; i < arr.size(); i++) {
                            vals[i] = arr.get(i).asText();
                        }
                        map.put(e.getKey(), vals);
                    }
                }
                pr.headers = map;
            }
            return pr;
        } catch (Exception e) {
            return null;
        }
    }

    private static String text(JsonNode n, String field) {
        JsonNode v = n.get(field);
        return (v == null || v.isNull()) ? null : v.asText();
    }

    static boolean validateStsUrl(String raw) {
        URI u;
        try {
            u = URI.create(raw);
        } catch (IllegalArgumentException e) {
            return false;
        }
        if (u.getScheme() == null || !u.getScheme().equals("https")) {
            return false;
        }
        String host = u.getHost();
        if (host == null || !STS_HOST.matcher(host).matches()) {
            return false;
        }
        return raw.contains("Action=GetCallerIdentity");
    }

    /**
     * Extracts the {@code <Arn>} value from the STS GetCallerIdentity XML,
     * ignoring the response namespace by matching on local name (mirrors the
     * C# {@code Descendants().FirstOrDefault(LocalName=="Arn")}). Hardened
     * against XXE: DTDs and external entities are disabled.
     */
    static String parseArn(String xml) {
        if (xml == null || xml.isEmpty()) {
            return null;
        }
        try {
            DocumentBuilderFactory dbf = DocumentBuilderFactory.newInstance();
            dbf.setNamespaceAware(true);
            dbf.setExpandEntityReferences(false);
            dbf.setFeature("http://apache.org/xml/features/disallow-doctype-decl", true);
            dbf.setFeature("http://xml.org/sax/features/external-general-entities", false);
            dbf.setFeature("http://xml.org/sax/features/external-parameter-entities", false);
            DocumentBuilder db = dbf.newDocumentBuilder();
            Document doc = db.parse(new ByteArrayInputStream(xml.getBytes(StandardCharsets.UTF_8)));
            return firstByLocalName(doc.getDocumentElement(), "Arn");
        } catch (Exception e) {
            return null;
        }
    }

    private static String firstByLocalName(Node node, String localName) {
        if (node == null) {
            return null;
        }
        if (node.getNodeType() == Node.ELEMENT_NODE) {
            Element el = (Element) node;
            String ln = el.getLocalName() != null ? el.getLocalName() : el.getNodeName();
            if (localName.equals(ln)) {
                return el.getTextContent();
            }
        }
        NodeList children = node.getChildNodes();
        for (int i = 0; i < children.getLength(); i++) {
            String found = firstByLocalName(children.item(i), localName);
            if (found != null) {
                return found;
            }
        }
        return null;
    }

    /**
     * Extracts the RoleSessionName from an assumed-role ARN
     * ({@code arn:aws:sts::<acct>:assumed-role/<role>/<sessionName>}). The
     * operator sets the session name to the agentId, so this is the AgentId
     * derivation — kept in lock step with Go {@code SessionNameFromArn} and the
     * C# {@code SessionNameFromArn} (the consistency invariant).
     */
    static String sessionNameFromArn(String arn) {
        if (arn == null) {
            return null;
        }
        int i = arn.indexOf(ASSUMED_ROLE_MARKER);
        if (i < 0) {
            return null;
        }
        String rest = arn.substring(i + ASSUMED_ROLE_MARKER.length());
        int slash = rest.indexOf('/');
        if (slash < 0) {
            return null;
        }
        String sessionName = rest.substring(slash + 1);
        return sessionName.isEmpty() ? null : sessionName;
    }

    /** The wire form of a SigV4-presigned GetCallerIdentity request. */
    static final class PresignedRequest {
        String method;
        String url;
        Map<String, String[]> headers;

        PresignedRequest() {
        }

        /** For tests: build a replay target directly. */
        PresignedRequest(String method, String url) {
            this.method = method;
            this.url = url;
        }
    }
}
