import { describe, expect, it } from 'vitest';
import { money } from './azure';
import type { GuardrailOrphan, GuardrailsView, Window } from './api';
import {
  budgetFiring,
  canDeleteOrphan,
  editableWindows,
  eveningsAndWeekends,
  isAlwaysOff,
  orphanLabel,
  parseThresholds,
  projectionFiring,
  projectionText,
  thresholdMarks,
  toggledDays,
  validWindows,
  windowsSavable,
  windowsSummary
} from './guardrails';

const view = (over: Partial<GuardrailsView>): GuardrailsView => ({
  budget: 100,
  spent: 40,
  currencies: ['EUR'],
  projection: null,
  projection_firing: false,
  days_billed: 10,
  thresholds: [],
  shares: [],
  orphans: [],
  events: [],
  timezone: 'Europe/Paris',
  ...over
});

const orphan = (over: Partial<GuardrailOrphan>): GuardrailOrphan => ({
  resource_id: '/s/d1',
  name: 'd1',
  type: 'Microsoft.Compute/disks',
  reason: 'disk_unattached',
  since: '2026-09-17T10:00:00Z',
  cost: 40,
  currency: 'EUR',
  deletable: true,
  ...over
});

describe('windowsSummary', () => {
  it('summarises windows the way a person would say them', () => {
    expect(windowsSummary(eveningsAndWeekends())).toBe('Mon–Fri 20:00–07:00, Sat–Sun all day');
    expect(windowsSummary([{ days: [1, 3], from: '22:00', to: '06:00' }])).toBe('Mon, Wed 22:00–06:00');
    expect(windowsSummary([])).toBe('no window');
  });

  it('reads a single day as itself, not a one-day range', () => {
    expect(windowsSummary([{ days: [2], from: '08:00', to: '09:00' }])).toBe('Tue 08:00–09:00');
  });
});

describe('validWindows', () => {
  it('accepts the evenings-and-weekends default', () => {
    expect(validWindows(eveningsAndWeekends())).toBeNull();
  });

  it('rejects a window with no day', () => {
    expect(validWindows([{ days: [], from: '20:00', to: '07:00' }])).toMatch(/day/);
  });

  it('rejects a malformed time', () => {
    expect(validWindows([{ days: [1], from: '20:00', to: '7:00' }])).toMatch(/HH:MM/);
  });

  it('rejects a day outside 1..7', () => {
    expect(validWindows([{ days: [0], from: '20:00', to: '07:00' }])).toMatch(/day/);
    expect(validWindows([{ days: [8], from: '20:00', to: '07:00' }])).toMatch(/day/);
  });

  it('rejects 24:00 as a start, and any other minute of hour 24', () => {
    expect(validWindows([{ days: [1], from: '24:00', to: '07:00' }])).toMatch(/HH:MM/);
    expect(validWindows([{ days: [1], from: '20:00', to: '24:30' }])).toMatch(/HH:MM/);
  });

  it('flags always-off as a notice, distinct from a structural problem', () => {
    const ws: Window[] = [{ days: [1, 2, 3, 4, 5, 6, 7], from: '00:00', to: '24:00' }];
    expect(validWindows(ws)).toMatch(/always off/);
    // The design spec (§1) accepts this configuration outright: it must not
    // block Save the way a real structural problem does.
    expect(windowsSavable(ws)).toBe(true);
  });
});

describe('windowsSavable', () => {
  it('blocks saving on a structural problem, not on the always-off notice', () => {
    expect(windowsSavable(eveningsAndWeekends())).toBe(true);
    expect(windowsSavable([{ days: [], from: '20:00', to: '07:00' }])).toBe(false);
    expect(windowsSavable([{ days: [1], from: '7:00', to: '20:00' }])).toBe(false);
  });
});

describe('isAlwaysOff', () => {
  it('is true only for one window covering every day, all day', () => {
    expect(isAlwaysOff([{ days: [1, 2, 3, 4, 5, 6, 7], from: '00:00', to: '24:00' }])).toBe(true);
    expect(isAlwaysOff(eveningsAndWeekends())).toBe(false);
    expect(isAlwaysOff([{ days: [1, 2, 3, 4, 5, 6, 7], from: '00:00', to: '23:59' }])).toBe(false);
  });

  it('reads the editor-safe "00:00" end-of-day spelling the same as "24:00"', () => {
    expect(isAlwaysOff([{ days: [1, 2, 3, 4, 5, 6, 7], from: '00:00', to: '00:00' }])).toBe(true);
  });
});

describe('editableWindows', () => {
  it('turns "24:00" into "00:00", the only end-of-day spelling a native time input can display', () => {
    const ws: Window[] = [{ days: [6, 7], from: '00:00', to: '24:00' }];
    expect(editableWindows(ws)).toEqual([{ days: [6, 7], from: '00:00', to: '00:00' }]);
  });

  it('leaves every other window untouched', () => {
    const ws: Window[] = [{ days: [1, 3], from: '20:00', to: '07:00' }];
    expect(editableWindows(ws)).toEqual(ws);
  });

  it("is what the evenings-and-weekends preset already returns, so it never renders a blank field", () => {
    // The preset mirrors guardrails.EveningsAndWeekends's own "24:00"
    // literal internally (see eveningsAndWeekends's own doc comment), but
    // must come out the other side already normalized — this is the
    // regression the flagship preset hit in review.
    expect(eveningsAndWeekends()).toEqual(editableWindows(eveningsAndWeekends()));
    expect(eveningsAndWeekends().some((w) => w.to === '24:00')).toBe(false);
  });

  it('round-trips a midnight-ending window through save-and-reload without losing the always-off meaning', () => {
    // Simulates what the page does: normalize on load (editableWindows),
    // send exactly that to the server (which stores "00:00" verbatim, since
    // ParseWindows/EncodeWindows round-trip whatever it's given), then load
    // it again — the window must still read as always-off both times.
    const saved = eveningsAndWeekends();
    const reloaded = editableWindows(saved); // a second load, same as the first
    expect(reloaded).toEqual(saved);
    expect(isAlwaysOff([{ days: [1, 2, 3, 4, 5, 6, 7], from: '00:00', to: reloaded[1].to }])).toBe(true);
  });
});

describe('projectionText', () => {
  it('renders an honest dash before there is enough billed data', () => {
    expect(projectionText(view({ projection: null, days_billed: 3, currencies: ['EUR'] }))).toBe('— (3 billed days)');
  });

  it('renders the real money() format, not a guessed one', () => {
    // money()'s exact punctuation depends on the runtime locale (confirmed:
    // this repo's default locale renders "118,00 €", not "€118"). Building
    // the expectation from money() itself, rather than a hand-typed string,
    // is what keeps this test honest about that — while still proving
    // projectionText rounds and wires the currency through correctly.
    const rendered = projectionText(view({ projection: 118.4, days_billed: 10, currencies: ['EUR'] }));
    expect(rendered).toBe(`${money(118, 'EUR')} by month end`);
  });

  it('names the count and sums across currencies rather than picking one', () => {
    const rendered = projectionText(view({ projection: 118.4, days_billed: 10, currencies: ['EUR', 'USD'] }));
    expect(rendered).toMatch(/2 currencies/);
    expect(rendered).toBe(`${money(118, undefined)} by month end (2 currencies summed)`);
  });
});

describe('projectionFiring / budgetFiring', () => {
  it('reads the server-computed projection_firing flag rather than recomputing the crossing', () => {
    // 99 sits inside the server's 95–105% hysteresis band (guardrails.Budget):
    // a client-side `projection > budget * 1.05` recompute would call both
    // of these "not firing", disagreeing with the journal whichever way the
    // flag actually reads. This is exactly the case that distinguishes
    // reading the flag from guessing at it.
    expect(projectionFiring(view({ budget: 100, projection: 99, projection_firing: true }))).toBe(true);
    expect(projectionFiring(view({ budget: 100, projection: 99, projection_firing: false }))).toBe(false);
  });

  it('follows the flag even past 100%, since only the journal tracks the band', () => {
    expect(projectionFiring(view({ budget: 100, projection: 120, projection_firing: false }))).toBe(false);
  });

  it('is true when a threshold fires even if the projection does not', () => {
    expect(
      budgetFiring(view({ thresholds: [{ pct: 80, line: 80, firing: true }], projection: null, projection_firing: false }))
    ).toBe(true);
  });

  it('is false when nothing fires', () => {
    expect(
      budgetFiring(view({ thresholds: [{ pct: 80, line: 80, firing: false }], projection: 50, projection_firing: false }))
    ).toBe(false);
  });
});

describe('toggledDays', () => {
  it('adds a day not present, keeping the list sorted', () => {
    expect(toggledDays([1, 5], 3)).toEqual([1, 3, 5]);
  });

  it('removes a day already present', () => {
    expect(toggledDays([1, 3, 5], 3)).toEqual([1, 5]);
  });
});

describe('thresholdMarks', () => {
  it('places threshold marks, dropping anything beyond the bar', () => {
    expect(thresholdMarks([{ pct: 80 }, { pct: 100 }, { pct: 150 }])).toEqual([80, 100]);
  });
});

describe('orphanLabel', () => {
  it('names every orphan reason the hub can produce', () => {
    expect(orphanLabel('disk_unattached')).toBe('Unattached disk');
    expect(orphanLabel('ip_unassociated')).toBe('Public IP not associated');
    expect(orphanLabel('nic_without_vm')).toBe('Network interface without a VM');
    expect(orphanLabel('plan_without_site')).toBe('App Service plan without a site');
    expect(orphanLabel('hub_vm_silent')).toBe('VM created by the hub, agent silent');
    expect(orphanLabel('unverified')).toBe('Not verified');
  });

  it('falls back to the raw reason rather than hiding an unknown one', () => {
    expect(orphanLabel('something_new')).toBe('something_new');
  });
});

describe('parseThresholds', () => {
  it('reads a comma-separated list, trimming stray whitespace', () => {
    expect(parseThresholds('80, 100')).toEqual([80, 100]);
  });

  it('drops the empty and the unparseable rather than rejecting the field', () => {
    expect(parseThresholds('80,,100, abc, 90')).toEqual([80, 100, 90]);
  });
});

describe('canDeleteOrphan', () => {
  it('allows a deletable, confirmed orphan', () => {
    expect(canDeleteOrphan(orphan({ deletable: true, reason: 'disk_unattached' }))).toBe(true);
  });

  it('refuses an undeletable kind, such as a public IP', () => {
    expect(canDeleteOrphan(orphan({ deletable: false, reason: 'ip_unassociated' }))).toBe(false);
  });

  it('refuses unverified even if the server says deletable — the invariant this whole lot leans on', () => {
    // A server that forgot its own override (task 10's deletable flag) must
    // not leak a working-looking Delete button here: unverified is checked
    // independently of whatever the server sent.
    expect(canDeleteOrphan(orphan({ deletable: true, reason: 'unverified' }))).toBe(false);
  });
});
