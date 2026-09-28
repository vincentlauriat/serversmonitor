import { describe, expect, it } from 'vitest';
import { canEdit, isLastLocalAdmin, providerLabel, roleLabel } from './users';
import type { UserRow } from './api';

const u = (o: Partial<UserRow>): UserRow => ({ id: 1, email: 'v@example.com', role: 'admin', provider: 'local', self: false, ...o });

describe('users', () => {
  it('labels roles and providers', () => {
    expect(roleLabel('viewer')).toBe('Read only');
    expect(roleLabel('admin')).toBe('Admin');
    expect(providerLabel('entra')).toBe('Microsoft');
  });
  it('lets only an admin edit', () => {
    expect(canEdit({ role: 'admin' })).toBe(true);
    expect(canEdit({ role: 'viewer' })).toBe(false);
    expect(canEdit(null)).toBe(false);
  });
  it('protects the last local admin only', () => {
    const local = u({ id: 1 });
    const entraAdmin = u({ id: 2, provider: 'entra' });
    expect(isLastLocalAdmin(local, [local, entraAdmin])).toBe(true);
    expect(isLastLocalAdmin(local, [local, u({ id: 3 })])).toBe(false);
    expect(isLastLocalAdmin(entraAdmin, [local, entraAdmin])).toBe(false);
  });
});
