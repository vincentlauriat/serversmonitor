import { describe, expect, it } from 'vitest';
import {
  actionOutcome,
  actionsFor,
  budgetShare,
  deleteConfirmed,
  isInFlight,
  leftovers,
  money,
  needsConfirmation,
  parseGroups,
  provisionBlockedReason,
  provisionOutcome,
  shortType,
  sortRows,
  toSettingsPayload,
  type AzureAction,
  type Provision,
  type ProvisionResource
} from './azure';
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
    provision_subnet_id: '',
    provision_hub_url: '',
    provision_size: '',
    provision_image: '',
    provision_admin_user: '',
    provision_ssh_key: '',
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

describe('actions', () => {
  const act = (o: Partial<AzureAction>): AzureAction => ({
    id: 1,
    resource_id: '/x',
    resource_name: 'app',
    action: 'stop',
    status: 'succeeded',
    requested_at: '2026-09-18T10:00:00Z',
    finished_at: null,
    error: '',
    state_before: null,
    state_after: null,
    origin: 'user',
    ...o
  });

  it('offers no buttons for a type with no verbs', () => {
    // Three buttons that all answer 400 are worse than none.
    expect(actionsFor('Microsoft.Web/serverfarms')).toEqual([]);
    expect(actionsFor('Microsoft.Web/sites')).toEqual(['start', 'stop', 'restart']);
    expect(actionsFor('microsoft.web/sites')).toEqual(['start', 'stop', 'restart']);
  });

  it('knows which resource is busy, and only that one', () => {
    const list = [act({ resource_id: '/a', status: 'running' }), act({ resource_id: '/b' })];
    expect(isInFlight('/a', list)).toBe(true);
    expect(isInFlight('/b', list)).toBe(false);
    expect(isInFlight('/c', list)).toBe(false);
  });

  it('says plainly that an interrupted action was never replayed', () => {
    expect(actionOutcome(act({ status: 'interrupted' }))).toMatch(/not replayed/);
  });

  it('does not claim a state it did not read back', () => {
    expect(actionOutcome(act({ status: 'succeeded', state_after: null }))).toMatch(/not read back/);
    expect(actionOutcome(act({ status: 'succeeded', state_after: 'Stopped' }))).toBe('now Stopped');
  });

  it('keeps Azure’s own message on a failure', () => {
    expect(actionOutcome(act({ status: 'failed', error: 'Forbidden: no Website Contributor' }))).toMatch(
      /Website Contributor/
    );
  });

  it('confirms what takes a site offline, and nothing else', () => {
    expect(needsConfirmation('stop')).toBe(true);
    expect(needsConfirmation('restart')).toBe(true);
    expect(needsConfirmation('start')).toBe(false);
  });
});

describe('provisioning', () => {
  const res = (o: Partial<ProvisionResource> = {}): ProvisionResource => ({
    arm_id: '/subs/x/providers/Microsoft.Network/networkInterfaces/vm1-nic',
    kind: 'nic',
    created_at: '2026-09-21T10:00:00Z',
    deleted_at: null,
    ...o
  });
  const prov = (o: Partial<Provision> = {}): Provision => ({
    id: 1,
    name: 'vm1',
    host_id: 7,
    status: 'succeeded',
    requested_at: '2026-09-21T10:00:00Z',
    finished_at: '2026-09-21T10:03:00Z',
    error: '',
    delete_error: '',
    resources: [res()],
    ...o
  });

  it('says plainly that an interrupted run was never resumed', () => {
    expect(provisionOutcome(prov({ status: 'interrupted' }))).toMatch(/not resumed/);
  });

  it('keeps Azure’s own reason on a failure', () => {
    expect(provisionOutcome(prov({ status: 'failed', error: 'quota exceeded' }))).toMatch(/quota/);
  });

  it('counts as leftovers only what nobody has deleted', () => {
    const p = prov({ resources: [res(), res({ arm_id: '/gone', deleted_at: '2026-09-21T11:00:00Z' })] });
    expect(leftovers(p)).toHaveLength(1);
    expect(leftovers(p)[0].arm_id).toMatch(/vm1-nic/);
  });

  it('requires the name typed back, and forgives only whitespace', () => {
    expect(deleteConfirmed('vm1', 'vm1')).toBe(true);
    expect(deleteConfirmed('  vm1 ', 'vm1')).toBe(true);
    expect(deleteConfirmed('vm', 'vm1')).toBe(false);
    expect(deleteConfirmed('VM1', 'vm1')).toBe(false);
    // An empty record name must never be confirmable by an empty box.
    expect(deleteConfirmed('', '')).toBe(false);
  });

  it('names why provisioning is unavailable rather than just greying out', () => {
    expect(provisionBlockedReason('off', true)).toMatch(/Settings/);
    expect(provisionBlockedReason('client_secret', false)).toMatch(/inventory/);
    expect(provisionBlockedReason('client_secret', true)).toBe('');
  });

  it('offers the same three buttons for a VM as for a site', () => {
    expect(actionsFor('Microsoft.Compute/virtualMachines')).toEqual(['start', 'stop', 'restart']);
  });
});
