using System.Collections.Concurrent;

namespace IdentityServer;

/// <summary>
/// Shared, singleton mapping of a CIBA request's InternalId to the registry's
/// ConsentRequest.ID. It must be a singleton because the services that read and
/// write it are transient: CibaConsentNotificationService records the mapping
/// when it creates the registry consent request, and ConsentCompletionPoller
/// consumes it to complete the Duende request once the registry resolves it.
///
/// In-memory and best-effort: a process restart between request-creation and
/// resolution loses the mapping (the registry request stays resolvable via its
/// own API/dashboard; only the automatic Duende completion is lost).
/// </summary>
public class ConsentRequestTracker
{
    private readonly ConcurrentDictionary<string, string> _ids = new();

    public void Track(string internalId, string registryRequestId) =>
        _ids[internalId] = registryRequestId;

    public bool TryGet(string internalId, out string registryRequestId) =>
        _ids.TryGetValue(internalId, out registryRequestId!);

    public void Untrack(string internalId) => _ids.TryRemove(internalId, out _);

    public IReadOnlyList<(string InternalId, string RegistryId)> Snapshot() =>
        _ids.Select(kv => (kv.Key, kv.Value)).ToList();
}
