// One SSE connection shared by every page; pages subscribe to event types.
type Listener = (data: unknown) => void;

class Live {
  connected = $state(false);
  private es: EventSource | null = null;
  private listeners = new Map<string, Set<Listener>>();
  /** Event names already wired to the EventSource. */
  private attached = new Set<string>();

  start() {
    if (this.es) return;
    this.es = new EventSource('/api/v1/events');
    this.es.onopen = () => (this.connected = true);
    this.es.onerror = () => (this.connected = false);
    // Whatever pages have already subscribed to, plus anything they subscribe
    // to later — see attach(). A hard-coded list here is how the Azure page's
    // live refresh sat dead from the day it shipped: the hub published `azure`
    // and nothing was listening for it.
    for (const type of this.listeners.keys()) this.attach(type);
  }

  private attach(type: string) {
    if (!this.es || this.attached.has(type)) return;
    this.attached.add(type);
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

  stop() {
    this.es?.close();
    this.es = null;
    this.attached.clear();
    this.connected = false;
  }

  on(type: string, fn: Listener): () => void {
    if (!this.listeners.has(type)) this.listeners.set(type, new Set());
    this.listeners.get(type)!.add(fn);
    this.attach(type); // a page that mounts after start() is still heard
    return () => {
      this.listeners.get(type)?.delete(fn);
    };
  }
}

export const live = new Live();
