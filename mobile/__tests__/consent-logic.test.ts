import { approveGuarded, narrowedScopes, toggleScope } from '../src/consent-logic';

describe('toggleScope', () => {
  it('adds and removes while preserving requested order', () => {
    const all = ['a:read', 'a:write', 'b:read'];
    expect(toggleScope(all, ['a:read'], 'b:read')).toEqual(['a:read', 'b:read']);
    expect(toggleScope(all, ['a:read', 'a:write'], 'a:read')).toEqual(['a:write']);
  });
});

describe('narrowedScopes', () => {
  it('returns undefined when the full set is kept (approve original scopes)', () => {
    expect(narrowedScopes(['a:read', 'a:write'], ['a:read', 'a:write'])).toBeUndefined();
    // order-insensitive
    expect(narrowedScopes(['a:read', 'a:write'], ['a:write', 'a:read'])).toBeUndefined();
  });
  it('returns the chosen subset when narrowed', () => {
    expect(narrowedScopes(['a:read', 'a:write'], ['a:read'])).toEqual(['a:read']);
  });
});

describe('approveGuarded — biometric gates approve (criterion #9)', () => {
  it('does NOT submit when biometric confirmation fails', async () => {
    const submit = jest.fn().mockResolvedValue(undefined);
    const confirm = jest.fn().mockResolvedValue(false); // user cancelled / failed
    const submitted = await approveGuarded({ confirm, submit }, ['a:read'], ['a:read']);
    expect(submitted).toBe(false);
    expect(submit).not.toHaveBeenCalled();
  });

  it('submits the narrowed subset after a successful biometric (criterion #5)', async () => {
    const submit = jest.fn().mockResolvedValue(undefined);
    const confirm = jest.fn().mockResolvedValue(true);
    const submitted = await approveGuarded({ confirm, submit }, ['a:read', 'a:write'], ['a:read']);
    expect(submitted).toBe(true);
    expect(submit).toHaveBeenCalledWith(['a:read']); // only the kept scope
  });

  it('submits undefined (original scopes) when nothing was narrowed', async () => {
    const submit = jest.fn().mockResolvedValue(undefined);
    const confirm = jest.fn().mockResolvedValue(true);
    await approveGuarded({ confirm, submit }, ['a:read', 'a:write'], ['a:read', 'a:write']);
    expect(submit).toHaveBeenCalledWith(undefined);
  });

  it('refuses to approve with an empty selection', async () => {
    const submit = jest.fn();
    const confirm = jest.fn().mockResolvedValue(true);
    await expect(approveGuarded({ confirm, submit }, ['a:read'], [])).rejects.toThrow();
    expect(confirm).not.toHaveBeenCalled();
    expect(submit).not.toHaveBeenCalled();
  });
});
