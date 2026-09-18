// One SSE connection shared by every page; pages subscribe to event types.
type Listener = (data: unknown) => void;

class Live {
  connected = $state(false);
  private es: EventSource | null = null;
  private listeners = new Map<string, Set<Listener>>();

  start() {
    if (this.es) return;
    this.es = new EventSource('/api/v1/events');
    this.es.onopen = () => (this.connected = true);
    this.es.onerror = () => (this.connected = false);
    for (const type of ['host', 'hosts', 'alert']) {
      this.es.addEventListener(type, (e) => {
        let data: unknown = null;
        try {
          data = JSON.parse((e as MessageEvent).data).data;
        } catch {
          return;
        }
        this.listeners.get(type)?.forEach((fn) => fn(data));
      });
    }
  }

  stop() {
    this.es?.close();
    this.es = null;
    this.connected = false;
  }

  on(type: string, fn: Listener): () => void {
    if (!this.listeners.has(type)) this.listeners.set(type, new Set());
    this.listeners.get(type)!.add(fn);
    return () => {
      this.listeners.get(type)?.delete(fn);
    };
  }
}

export const live = new Live();
