import type { Me, UserRow } from './api';

/** What a role lets a person do, in the words the page uses. */
export function roleLabel(role: string): string {
  return role === 'admin' ? 'Admin' : 'Read only';
}

export function providerLabel(provider: string): string {
  return provider === 'entra' ? 'Microsoft' : 'Password';
}

/** Whether the page should offer anything that changes something. */
export function canEdit(me: Pick<Me, 'role'> | null): boolean {
  return me?.role === 'admin';
}

/**
 * The one account whose role and removal the hub refuses to change: the last
 * local admin, which is the way in when Microsoft sign-in is down. The page
 * greys its buttons rather than letting the server say no.
 */
export function isLastLocalAdmin(u: UserRow, all: UserRow[]): boolean {
  if (u.provider !== 'local' || u.role !== 'admin') return false;
  return all.filter((x) => x.provider === 'local' && x.role === 'admin').length === 1;
}

/** The Microsoft sign-in link carries nothing: the hub builds the whole URL. */
export const ENTRA_START = '/api/v1/auth/entra/start';
