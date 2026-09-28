import type { Notifications, NotifyHealth } from './api';

/** Splits a textarea of recipients on commas or newlines, dropping blanks. */
export function parseRecipients(raw: string): string[] {
  return raw
    .split(/[,\n]/)
    .map((s) => s.trim())
    .filter((s) => s.length > 0);
}

/**
 * Reads one `Name: value` header per line. A line without a name is dropped
 * rather than sent as an empty key, which the server would reject as a whole.
 */
export function parseHeaders(raw: string): Record<string, string> {
  const out: Record<string, string> = {};
  for (const line of raw.split('\n')) {
    const i = line.indexOf(':');
    if (i <= 0) continue;
    const name = line.slice(0, i).trim();
    if (name) out[name] = line.slice(i + 1).trim();
  }
  return out;
}

/**
 * Builds the PUT body. The password is the only field the server sends back as
 * a boolean, so it is the only one carried separately: null means "leave the
 * stored one alone", a string means "use this", including the empty string,
 * which clears it.
 */
export function toPayload(v: Notifications, password: string | null): Record<string, unknown> {
  const { smtp_password_set: _set, health: _health, ...rest } = v;
  const out: Record<string, unknown> = { ...rest };
  if (password !== null) out.smtp_password = password;
  return out;
}

/** Says whether a channel has ever worked, which "configured" does not answer. */
export function healthLabel(h: NotifyHealth | undefined): string {
  if (!h) return 'never used';
  if (h.state === 'sent') return 'last delivery succeeded';
  if (h.state === 'pending') return 'delivery in flight';
  return `last delivery failed: ${h.last_error ?? 'no reason given'}`;
}

/** Every channel the hub knows, in the order the server normalises routes to. */
export const CHANNELS: [string, string][] = [
  ['smtp', 'Email'],
  ['webhook', 'Webhook'],
  ['teams', 'Teams']
];

/** The channels switched on in the saved notification settings. */
export function enabledChannels(v: Notifications): string[] {
  const on: Record<string, boolean> = { smtp: v.smtp_enabled, webhook: v.webhook_enabled, teams: v.teams_enabled };
  return CHANNELS.map(([k]) => k).filter((k) => on[k]);
}

/**
 * Says where a route sends things. A channel named but switched off is kept in
 * the route, so it is shown, marked "(off)": the rule is not broken, it is
 * waiting for that channel.
 */
export function routeLabel(route: string[] | null, enabled: string[]): string {
  if (route === null) return 'All channels';
  if (route.length === 0) return 'None';
  return CHANNELS.filter(([k]) => route.includes(k))
    .map(([k, label]) => (enabled.includes(k) ? label : `${label} (off)`))
    .join(', ');
}

/** Adds or removes one channel, keeping the channel order. */
export function toggleChannel(route: string[], channel: string): string[] {
  const next = route.includes(channel) ? route.filter((c) => c !== channel) : [...route, channel];
  return CHANNELS.map(([k]) => k).filter((k) => next.includes(k));
}
