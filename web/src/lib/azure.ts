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
