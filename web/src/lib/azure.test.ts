import { describe, expect, it } from 'vitest';
import { budgetShare, money, parseGroups, shortType, sortRows, toSettingsPayload } from './azure';
import type { AzureRow, AzureSettings } from './api';

const row = (over: Partial<AzureRow>): AzureRow => ({
  id: 'x',
  name: 'x',
  type: 'Microsoft.Web/sites',
  resource_group: 'rg',
  location: 'we',
  state: 'Running',
  tags: {},
  cost: null,
  deleted: false,
  ...over
});

describe('money', () => {
  it('renders a dash when Azure has reported nothing', () => {
    // A cost Azure has not reported is not zero.
    expect(money(null, 'EUR')).toBe('—');
  });
  it('renders zero as zero', () => {
    expect(money(0, 'EUR')).not.toBe('—');
  });
  it('keeps small amounts visible instead of rounding them away', () => {
    // The sandbox's real numbers are fractions of a cent; 0,00 € reads as free.
    expect(money(0.000838, 'EUR')).not.toMatch(/^0[.,]00\s*€?$/);
  });
  it('carries the currency', () => {
    expect(money(3.5, 'USD')).toContain('3');
  });
});

describe('shortType', () => {
  it('drops the provider prefix', () => {
    expect(shortType('Microsoft.Web/sites')).toBe('sites');
    expect(shortType('Microsoft.Web/serverfarms')).toBe('serverfarms');
  });
  it('survives a type it has never seen', () => {
    expect(shortType('')).toBe('');
    expect(shortType('weird')).toBe('weird');
  });
});

describe('budgetShare', () => {
  it('is null when no budget is set', () => {
    expect(budgetShare(5, 0)).toBeNull();
  });
  it('is a fraction of the budget', () => {
    expect(budgetShare(25, 100)).toBeCloseTo(0.25);
  });
  it('does not cap at one, because going over budget is the thing worth seeing', () => {
    expect(budgetShare(150, 100)).toBeCloseTo(1.5);
  });
});

describe('sortRows', () => {
  it('puts deleted resources last whatever their cost', () => {
    const rows = [row({ name: 'gone', deleted: true, cost: 100 }), row({ name: 'alive', cost: 1 })];
    expect(sortRows(rows).map((r) => r.name)).toEqual(['alive', 'gone']);
  });
  it('orders the living by cost, most expensive first', () => {
    const rows = [row({ name: 'cheap', cost: 1 }), row({ name: 'dear', cost: 9 })];
    expect(sortRows(rows).map((r) => r.name)).toEqual(['dear', 'cheap']);
  });
  it('puts a resource with no reported cost after the ones that have one', () => {
    const rows = [row({ name: 'unknown', cost: null }), row({ name: 'cheap', cost: 0.01 })];
    expect(sortRows(rows).map((r) => r.name)).toEqual(['cheap', 'unknown']);
  });
  it('does not mutate its input', () => {
    const rows = [row({ name: 'b', cost: 1 }), row({ name: 'a', cost: 2 })];
    sortRows(rows);
    expect(rows[0].name).toBe('b');
  });
});

describe('parseGroups', () => {
  it('splits on commas and newlines and drops blanks', () => {
    expect(parseGroups('rg-a,\n rg-b \n\n')).toEqual(['rg-a', 'rg-b']);
  });
  it('turns nothing into an empty list, which the server refuses on purpose', () => {
    expect(parseGroups('  \n ')).toEqual([]);
  });
});

describe('toSettingsPayload', () => {
  const s: AzureSettings = {
    mode: 'client_secret',
    tenant_id: 't',
    client_id: 'c',
    client_secret_set: true,
    mi_client_id: '',
    subscription_id: 's',
    resource_groups: [],
    inventory_every_min: 15,
    cost_every_min: 60,
    budget_monthly: 0
  };
  it('never sends back the read-only flag', () => {
    expect(toSettingsPayload(s, ['rg'], null)).not.toHaveProperty('client_secret_set');
  });
  it('omits the secret entirely when it was not touched', () => {
    // Absent means "keep the stored one". Sending '' would clear it.
    expect(toSettingsPayload(s, ['rg'], null)).not.toHaveProperty('client_secret');
  });
  it('sends an empty secret when the field was emptied on purpose', () => {
    expect(toSettingsPayload(s, ['rg'], '')).toHaveProperty('client_secret', '');
  });
  it('carries the groups the textarea produced, not the stale ones', () => {
    expect(toSettingsPayload(s, ['rg-new'], null).resource_groups).toEqual(['rg-new']);
  });
});
