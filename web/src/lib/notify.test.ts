import { describe, expect, it } from 'vitest';
import { healthLabel, parseHeaders, parseRecipients, toPayload } from './notify';
import type { Notifications } from './api';

const base: Notifications = {
  public_url: 'https://hub.example',
  smtp_enabled: true,
  smtp_host: 'smtp.example',
  smtp_port: 587,
  smtp_username: 'u',
  smtp_password_set: true,
  smtp_from: 'hub@example',
  smtp_to: ['a@example'],
  smtp_tls: 'starttls',
  webhook_enabled: false,
  webhook_url: '',
  webhook_headers: {},
  teams_enabled: false,
  teams_url: '',
  health: {}
};

describe('toPayload', () => {
  it('omits the password when the field was not touched', () => {
    expect('smtp_password' in toPayload(base, null)).toBe(false);
  });

  it('sends the password when the user typed one', () => {
    expect(toPayload(base, 'hunter2').smtp_password).toBe('hunter2');
  });

  it('sends an empty string when the user cleared it', () => {
    const p = toPayload(base, '');
    expect('smtp_password' in p).toBe(true);
    expect(p.smtp_password).toBe('');
  });

  it('never sends the read-only fields back', () => {
    const p = toPayload(base, null);
    expect('smtp_password_set' in p).toBe(false);
    expect('health' in p).toBe(false);
  });

  it('keeps everything else', () => {
    const p = toPayload(base, null);
    expect(p.smtp_host).toBe('smtp.example');
    expect(p.smtp_to).toEqual(['a@example']);
    expect(p.public_url).toBe('https://hub.example');
  });
});

describe('parseRecipients', () => {
  it('splits on commas and newlines and drops blanks', () => {
    expect(parseRecipients(' a@x ,\n b@x ,,\n')).toEqual(['a@x', 'b@x']);
  });
  it('returns an empty list for empty input', () => {
    expect(parseRecipients('   ')).toEqual([]);
  });
});

describe('parseHeaders', () => {
  it('reads one header per line', () => {
    expect(parseHeaders('Authorization: Bearer t\nX-Topic: alerts')).toEqual({
      Authorization: 'Bearer t',
      'X-Topic': 'alerts'
    });
  });
  it('keeps colons inside the value', () => {
    expect(parseHeaders('X-Url: https://example/x')).toEqual({ 'X-Url': 'https://example/x' });
  });
  it('ignores blank and malformed lines rather than sending an empty key', () => {
    expect(parseHeaders('\n  \nnotaheader\n: novalue\n')).toEqual({});
  });
});

describe('healthLabel', () => {
  it('says nothing has been sent yet when there is no history', () => {
    expect(healthLabel(undefined)).toBe('never used');
  });
  it('reports the failure reason', () => {
    expect(healthLabel({ state: 'failed', last_error: '503', at: '2026-09-18T03:00:00Z' })).toContain('503');
  });
  it('reports success plainly', () => {
    expect(healthLabel({ state: 'sent', at: '2026-09-18T03:00:00Z' })).toBe('last delivery succeeded');
  });
  it('does not invent a reason when the server gave none', () => {
    expect(healthLabel({ state: 'failed', at: '2026-09-18T03:00:00Z' })).toContain('no reason given');
  });
});
