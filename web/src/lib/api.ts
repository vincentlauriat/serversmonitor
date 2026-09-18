export interface Disk {
  mount: string;
  used: number;
  total: number;
  read_bps: number;
  write_bps: number;
}
export interface Temp {
  sensor: string;
  celsius: number;
}
export interface Latest {
  at: string;
  cpu: number | null;
  mem_used: number | null;
  mem_total: number | null;
  swap_used: number | null;
  swap_total: number | null;
  load1: number | null;
  load5: number | null;
  load15: number | null;
  uptime: number | null;
  net_sent_bps: number | null;
  net_recv_bps: number | null;
  disks: Disk[] | null;
  temps: Temp[] | null;
}
export interface Host {
  id: number;
  name: string;
  status: 'online' | 'offline' | 'never_seen';
  last_seen: string | null;
  os: string;
  arch: string;
  hostname: string;
  agent_version: string;
  cores: number;
  mem_total: number;
  muted: boolean;
  connected: boolean;
  latest: Latest | null;
  firing: number;
}
export interface Point {
  at: string;
  cpu: number | null;
  cpu_max: number | null;
  mem_used: number | null;
  mem_total: number | null;
  swap_used: number | null;
  load1: number | null;
  load5: number | null;
  load15: number | null;
  net_sent_bps: number | null;
  net_recv_bps: number | null;
  disks: Disk[] | null;
  temps: Temp[] | null;
}
export interface Container {
  name: string;
  image: string;
  status: string;
  cpu: number | null;
  mem_used: number | null;
  net_sent_bps: number | null;
  net_recv_bps: number | null;
  updated_at: string;
}
export interface Rule {
  id: number;
  host_id: number | null;
  metric: string;
  threshold: number;
  duration_sec: number;
}
export interface AlertEvent {
  id: number;
  rule_id: number;
  host_id: number;
  host_name: string;
  metric: string;
  kind: 'fired' | 'resolved';
  value: number;
  at: string;
}
export interface Settings {
  agent_interval_sec: number;
  retention_raw_hours: number;
  retention_10m_days: number;
  retention_1h_days: number;
  version?: string;
}

export class ApiError extends Error {
  constructor(
    public status: number,
    message: string,
    public setupRequired = false
  ) {
    super(message);
  }
}

async function call<T>(method: string, path: string, body?: unknown): Promise<T> {
  const res = await fetch(path, {
    method,
    headers: body === undefined ? {} : { 'Content-Type': 'application/json' },
    body: body === undefined ? undefined : JSON.stringify(body)
  });
  if (res.status === 204) return undefined as T;
  const data = await res.json().catch(() => ({}));
  if (!res.ok) throw new ApiError(res.status, data.error ?? res.statusText, data.setup_required === true);
  return data as T;
}

export const api = {
  get: <T>(path: string) => call<T>('GET', path),
  post: <T>(path: string, body?: unknown) => call<T>('POST', path, body),
  put: <T>(path: string, body?: unknown) => call<T>('PUT', path, body),
  patch: <T>(path: string, body?: unknown) => call<T>('PATCH', path, body),
  del: <T>(path: string) => call<T>('DELETE', path)
};

export interface NotifyHealth {
  state: string;
  last_error?: string;
  at: string;
}
export interface Notifications {
  public_url: string;
  smtp_enabled: boolean;
  smtp_host: string;
  smtp_port: number;
  smtp_username: string;
  smtp_password_set: boolean;
  smtp_from: string;
  smtp_to: string[];
  smtp_tls: string;
  webhook_enabled: boolean;
  webhook_url: string;
  webhook_headers: Record<string, string>;
  teams_enabled: boolean;
  teams_url: string;
  health: Record<string, NotifyHealth>;
}
export interface Delivery {
  id: number;
  channel: string;
  state: string;
  attempts: number;
  last_error?: string;
  host: string;
  metric: string;
  kind: string;
  at: string;
}
