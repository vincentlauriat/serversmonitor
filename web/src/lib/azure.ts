import type { AzureRow, AzureSettings } from './api';

/**
 * Renders an amount, or a dash when Azure has reported nothing for it.
 * A missing cost is not a zero cost, and the two must not look alike.
 *
 * Small amounts keep enough digits to stay visible: the sandbox's real numbers
 * are fractions of a cent, and "0,00 €" reads as free.
 */
export function money(amount: number | null, currency: string | undefined): string {
  if (amount === null || amount === undefined) return '—';
  const digits = amount !== 0 && Math.abs(amount) < 0.01 ? 4 : 2;
  try {
    return new Intl.NumberFormat(undefined, {
      style: currency ? 'currency' : 'decimal',
      currency: currency || undefined,
      minimumFractionDigits: digits,
      maximumFractionDigits: digits
    }).format(amount);
  } catch {
    // An unknown currency code throws rather than degrading, and a table cell
    // that throws takes the whole page with it.
    return `${amount.toFixed(digits)} ${currency ?? ''}`.trim();
  }
}

/** Drops the provider prefix: Microsoft.Web/sites reads as sites. */
export function shortType(t: string): string {
  const i = t.indexOf('/');
  return i >= 0 ? t.slice(i + 1) : t;
}

/**
 * The share of the budget spent, or null when no budget is set. Not capped:
 * going over is exactly the thing worth seeing.
 *
 * Takes the two numbers rather than a total, because the budget belongs to the
 * hub and the spend belongs to a currency. Passing a total would invite showing
 * the same budget beside a euro figure and a dollar one.
 */
export function budgetShare(spent: number, budget: number): number | null {
  if (!budget) return null;
  return spent / budget;
}

/**
 * Living resources first, dearest first; a resource with no reported cost sorts
 * after the ones that have one, then deleted resources last. Returns a new
 * array: sorting in place would reorder the state the page is rendering from.
 */
export function sortRows(rows: AzureRow[]): AzureRow[] {
  return [...rows].sort((a, b) => {
    if (a.deleted !== b.deleted) return a.deleted ? 1 : -1;
    if ((a.cost === null) !== (b.cost === null)) return a.cost === null ? 1 : -1;
    if (a.cost !== null && b.cost !== null && a.cost !== b.cost) return b.cost - a.cost;
    return a.name.localeCompare(b.name);
  });
}

/**
 * Reads resource group names from a textarea, on commas or newlines. Written
 * here rather than reused from the notification helpers: a package that reads
 * Azure has no business depending on the one that sends mail, and the server
 * side draws the same line for the same reason.
 */
export function parseGroups(raw: string): string[] {
  return raw
    .split(/[,\n]/)
    .map((s) => s.trim())
    .filter((s) => s.length > 0);
}

/**
 * Builds the PUT body. The secret is the only field the server sends back as a
 * boolean, so it is the only one carried separately: null means "leave the
 * stored one alone", a string means "use this", including the empty string,
 * which clears it.
 */
export function toSettingsPayload(
  v: AzureSettings,
  groups: string[],
  secret: string | null
): Record<string, unknown> {
  const { client_secret_set: _set, ...rest } = v;
  const out: Record<string, unknown> = { ...rest, resource_groups: groups };
  if (secret !== null) out.client_secret = secret;
  return out;
}

/** One start, stop or restart, as the action log records it. */
export interface AzureAction {
  id: number;
  resource_id: string;
  resource_name: string;
  action: string;
  status: 'pending' | 'running' | 'succeeded' | 'failed' | 'interrupted';
  requested_at: string;
  finished_at: string | null;
  error: string;
  state_before: string | null;
  state_after: string | null;
  /** "user" or "schedule" — who asked for this action. */
  origin: string;
}

/**
 * The actions a resource type accepts. Empty for a type the hub has no verbs
 * for — showing three buttons that all answer 400 would be worse than showing
 * none. Kept in step with azure.Actionable on the Go side, deliberately by
 * hand: two short lists that disagree are visible, a generated one is not.
 */
export function actionsFor(resourceType: string): string[] {
  switch (resourceType.toLowerCase()) {
    case 'microsoft.web/sites':
    case 'microsoft.compute/virtualmachines':
      return ['start', 'stop', 'restart'];
    default:
      return [];
  }
}

/** True while this resource has an action the hub has not finished. */
export function isInFlight(resourceID: string, actions: AzureAction[]): boolean {
  return actions.some(
    (a) => a.resource_id === resourceID && (a.status === 'pending' || a.status === 'running')
  );
}

/**
 * What the log says happened. An interrupted action is the honest one: the hub
 * died mid-flight and never replayed it, so nobody knows whether Azure acted.
 */
export function actionOutcome(a: AzureAction): string {
  switch (a.status) {
    case 'pending':
    case 'running':
      return 'in progress';
    case 'succeeded':
      return a.state_after ? `now ${a.state_after}` : 'done, state not read back';
    case 'failed':
      return a.error || 'failed';
    case 'interrupted':
      return 'the hub stopped mid-action; it was not replayed';
    default:
      return a.status;
  }
}

/** Stopping or restarting takes a running site offline; starting one cannot. */
export function needsConfirmation(action: string): boolean {
  return action === 'stop' || action === 'restart';
}

/** One resource a provision created, as the record holds it. */
export interface ProvisionResource {
  arm_id: string;
  kind: string;
  created_at: string;
  deleted_at: string | null;
}

/** One attempt at creating a VM. */
export interface Provision {
  id: number;
  name: string;
  host_id: number | null;
  status: 'pending' | 'running' | 'succeeded' | 'failed' | 'interrupted';
  requested_at: string;
  finished_at: string | null;
  error: string;
  delete_error: string;
  resources: ProvisionResource[];
}

/**
 * What the record says happened. An interrupted run is the one worth spelling
 * out: the hub died mid-creation and never resumed, so whatever it had
 * already created is still there and still costing money.
 */
export function provisionOutcome(p: Provision): string {
  switch (p.status) {
    case 'pending':
    case 'running':
      return 'creating…';
    case 'succeeded':
      return 'created';
    case 'failed':
      return p.error || 'failed';
    case 'interrupted':
      return 'the hub stopped mid-creation; it was not resumed';
    default:
      return p.status;
  }
}

/**
 * The resources a run created and nobody has deleted. These are what a failed
 * or interrupted run leaves behind, and what the bill is made of.
 */
export function leftovers(p: Provision): ProvisionResource[] {
  return p.resources.filter((r) => r.deleted_at === null);
}

/**
 * Deleting is confirmed by typing the name, not by a dialog dismissed by
 * reflex. Surrounding spaces are forgiven — a pasted name often carries one —
 * and nothing else is.
 */
export function deleteConfirmed(typed: string, name: string): boolean {
  return typed.trim() === name && name !== '';
}

/**
 * Whether the New VM form can be used, and what to say when it cannot. The
 * message names the setting to go and fill in: a greyed-out button that
 * explains nothing is the failure this replaces.
 */
export function provisionBlockedReason(
  mode: string | undefined,
  inventoryOK: boolean | undefined
): string {
  if (!mode || mode === 'off') return 'Azure is off. Configure it under Settings → Azure.';
  if (inventoryOK !== true) {
    return 'The last inventory did not succeed, so the hub cannot show what it would create.';
  }
  return '';
}
