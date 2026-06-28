// Pure consent logic, framework-free so it is unit-testable without a React
// Native render harness. The screens call into these.

// toggleScope flips one scope in the selected set, preserving the original
// order from the request's scope list.
export function toggleScope(all: string[], selected: string[], scope: string): string[] {
  const set = new Set(selected);
  if (set.has(scope)) set.delete(scope);
  else set.add(scope);
  return all.filter((s) => set.has(s));
}

// narrowedScopes returns the scopes to send on approve: undefined when the user
// kept the full requested set (the gateway then approves the original scopes),
// or the chosen subset when they narrowed it. An empty selection is invalid —
// the caller disables Approve, but we surface it as [] so a bug can't silently
// approve everything.
export function narrowedScopes(requested: string[], selected: string[]): string[] | undefined {
  if (selected.length === requested.length && requested.every((s) => selected.includes(s))) {
    return undefined; // unchanged → approve the originally-requested scopes
  }
  return selected;
}

export interface GuardedApproveDeps {
  // confirm runs the biometric/passcode gate; must resolve true to proceed.
  confirm: (reason: string) => Promise<boolean>;
  // submit performs the actual approve API call.
  submit: (scopes?: string[]) => Promise<void>;
}

// approveGuarded enforces that an approval is ONLY submitted after a successful
// local authentication. This is the single chokepoint the UI uses, so the
// "biometric gates approve" guarantee is verifiable in isolation. Returns true
// when the approval was submitted, false when the user failed/cancelled the
// biometric prompt (submit is never called in that case).
export async function approveGuarded(
  deps: GuardedApproveDeps,
  requested: string[],
  selected: string[],
): Promise<boolean> {
  if (selected.length === 0) {
    throw new Error('cannot approve with no scopes selected');
  }
  const ok = await deps.confirm('Approve agent authorization');
  if (!ok) return false;
  await deps.submit(narrowedScopes(requested, selected));
  return true;
}
