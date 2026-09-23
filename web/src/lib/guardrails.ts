import { money } from './azure';
import type { GuardrailsView, Window } from './api';

/**
 * ISO day numbering used throughout: 1 = Monday … 7 = Sunday, matching
 * guardrails.Window on the Go side exactly — a mismatched index here would
 * put the wrong day on the wrong checkbox with nothing to catch it.
 */
const DAY = ['', 'Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat', 'Sun'];

/** DAY, exported so the schedule editor's seven checkboxes use the exact
 * same labels and index convention as windowsSummary — two lists that
 * disagree would be visible on the page and wrong on every render. */
export const DAY_LABELS: readonly string[] = DAY;

/**
 * Reads budget thresholds from a comma-separated text field, the same
 * forgiving parse parseGroups uses for resource groups: trims, drops the
 * empty and the unparseable rather than rejecting the whole field for one
 * stray comma. Validation of the surviving numbers (1–500) is the server's
 * job (guardrails.Settings.Validate), same as every other Azure setting.
 */
export function parseThresholds(raw: string): number[] {
  return raw
    .split(',')
    .map((s) => s.trim())
    .filter((s) => s.length > 0)
    .map(Number)
    .filter((n) => Number.isFinite(n));
}

/**
 * The common default: off outside business hours on weekdays, off all
 * weekend. Mirrors guardrails.EveningsAndWeekends on the Go side field for
 * field.
 */
export function eveningsAndWeekends(): Window[] {
  return [
    { days: [1, 2, 3, 4, 5], from: '20:00', to: '07:00' },
    { days: [6, 7], from: '00:00', to: '24:00' }
  ];
}

function dayRange(days: number[]): string {
  const d = [...days].sort((a, b) => a - b);
  const contiguous = d.every((x, i) => i === 0 || x === d[i - 1] + 1);
  if (contiguous && d.length >= 2) return `${DAY[d[0]]}–${DAY[d[d.length - 1]]}`;
  return d.map((x) => DAY[x]).join(', ');
}

/**
 * How a person would say the windows out loud. Empty reads as "no window",
 * never as a blank cell that could be mistaken for "still loading".
 */
export function windowsSummary(ws: Window[]): string {
  if (ws.length === 0) return 'no window';
  return ws
    .map((w) => `${dayRange(w.days)} ${w.from === '00:00' && w.to === '24:00' ? 'all day' : `${w.from}–${w.to}`}`)
    .join(', ');
}

/**
 * HH:MM, mirroring guardrails.minutes on the Go side: hour 0–24, minute
 * 0–59, and hour 24 only ever means "24:00" (end of day) — never a window's
 * start, and never any other minute of the 24th hour ("24:30" is not a time
 * of day on either side).
 */
function validTime(s: string, end: boolean): boolean {
  if (!/^([01]\d|2[0-3]|24):[0-5]\d$/.test(s)) return false;
  const hh = Number(s.slice(0, 2));
  const mm = Number(s.slice(3));
  return hh !== 24 || (mm === 0 && end);
}

/**
 * The first structural problem with the windows — a shape a person must fix
 * before saving. Kept separate from the "always off" notice below, which is
 * a valid, if unusual, configuration the design spec explicitly accepts
 * (§1: "a window covering all of every day is accepted").
 */
function structuralProblem(ws: Window[]): string | null {
  for (const w of ws) {
    if (!w.days || w.days.length === 0) return 'every window needs at least one day';
    if (w.days.some((d) => d < 1 || d > 7)) return 'day must be between 1 (Monday) and 7 (Sunday)';
    if (!validTime(w.from, false) || !validTime(w.to, true)) return 'times are HH:MM';
  }
  return null;
}

/**
 * True when a single window covers every day, all day: the resource would
 * never turn back on by the schedule alone. Accepted server-side (an "off
 * until further notice" schedule), so this is a notice, not an error — see
 * windowsSavable.
 */
export function isAlwaysOff(ws: Window[]): boolean {
  return ws.some((w) => w.from === '00:00' && w.to === '24:00' && w.days.length === 7);
}

/**
 * null when the windows are fine, else the first problem — a structural one
 * a person must fix, or the always-off notice. This is what the editor shows
 * live; use windowsSavable to decide whether Save is enabled, since the
 * always-off case must not block saving.
 */
export function validWindows(ws: Window[]): string | null {
  const problem = structuralProblem(ws);
  if (problem) return problem;
  if (isAlwaysOff(ws)) return 'this resource would be always off — fine if that is what you want';
  return null;
}

/**
 * Whether the form can be saved. Only a structural problem blocks it: the
 * always-off case is a notice (see isAlwaysOff), never a reason to disable
 * Save.
 */
export function windowsSavable(ws: Window[]): boolean {
  return structuralProblem(ws) === null;
}

/**
 * Toggles one day in a window's day list, keeping it sorted. Exported and
 * tested on its own rather than left as an inline handler: there is no
 * component-test setup in this repo (see canDeleteOrphan's comment above),
 * so any editor logic worth getting right on click needs to live in a plain
 * function like this one, and the page writes the result back with a whole
 * new array (`editWindows = editWindows.map(...)`) rather than mutating a
 * loop-local binding in place, so a checkbox click is never in doubt about
 * whether it reached the state Save will send.
 */
export function toggledDays(days: number[], d: number): number[] {
  return days.includes(d) ? days.filter((x) => x !== d) : [...days, d].sort((a, b) => a - b);
}

/** The percentages worth drawing as marks on the bar: beyond 100 % there is
 * no bar left to put a mark on. */
export function thresholdMarks(thresholds: { pct: number }[]): number[] {
  return thresholds.map((t) => t.pct).filter((p) => p <= 100);
}

/**
 * Renders the projected end-of-month spend, or an honest dash before there
 * is enough billed data to project from (guardrails.Projection's own rule:
 * fewer than four billed days, or no cost data at all). An absent
 * projection is never a zero.
 */
export function projectionText(v: GuardrailsView): string {
  if (v.projection === null || v.projection === undefined) {
    return `— (${v.days_billed} billed day${v.days_billed === 1 ? '' : 's'})`;
  }
  const currency = v.currencies.length === 1 ? v.currencies[0] : undefined;
  const amount = money(Math.round(v.projection), currency);
  return v.currencies.length > 1
    ? `${amount} by month end (${v.currencies.length} currencies summed)`
    : `${amount} by month end`;
}

/**
 * Whether the projection rule is currently firing. The view carries a
 * `firing` flag per budget_threshold instance but not for budget_projection,
 * which has its own 5 % hysteresis band server-side (guardrails.Budget).
 * Rather than guess at that hysteresis state from the truncated 20-event
 * list, this reads the plain crossing the rule fires on — the same number,
 * without the resolve-side deadband. The bar can stay grey a fraction of a
 * percent longer than the server would on the way back down; the event log
 * below is the source of truth for exactly when it fired and resolved.
 */
export function projectionFiring(v: GuardrailsView): boolean {
  return v.budget > 0 && v.projection !== null && v.projection !== undefined && v.projection > v.budget * 1.05;
}

/** Whether the budget bar itself should read as firing: any threshold, or
 * the projection. */
export function budgetFiring(v: GuardrailsView): boolean {
  return v.thresholds.some((t) => t.firing) || projectionFiring(v);
}

const ORPHAN_LABELS: Record<string, string> = {
  disk_unattached: 'Unattached disk',
  ip_unassociated: 'Public IP not associated',
  nic_without_vm: 'Network interface without a VM',
  plan_without_site: 'App Service plan without a site',
  hub_vm_silent: 'VM created by the hub, agent silent',
  unverified: 'Not verified'
};

/** A person-readable name for an orphan reason. Falls back to the raw
 * string for a reason this page has never heard of, rather than hiding it. */
export function orphanLabel(reason: string): string {
  return ORPHAN_LABELS[reason] ?? reason;
}

/**
 * Whether an orphan row may offer a Delete button. The API already refuses
 * an unverified resource server-side — guardrails is never sure enough
 * about something it could not confirm to let it be deleted — but the page
 * must not offer a button that cannot succeed. This is checked independently
 * of whatever `deletable` the server sent, so a server that forgot its own
 * override does not leak a working-looking button here: "unverified" can
 * never be deleted through this page, full stop.
 */
export function canDeleteOrphan(o: { reason: string; deletable: boolean }): boolean {
  return o.deletable && o.reason !== 'unverified';
}
