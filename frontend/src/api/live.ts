import type { InfiniteData, QueryClient } from "@tanstack/react-query";
import { useEffect, useSyncExternalStore } from "react";
import type { AuditEntry, AuditPage, Camera, EventItem, EventPage, NodeMetrics, NodeSample } from "./client";
import { type AuditFilter, auditMatches, keys } from "./queries";

/** A message of the WebSocket: every topic numbers its messages. */
interface Message {
  topic: string;
  seq: number;
  type: string;
  at: number;
  data?: unknown;
}

type Status = "connecting" | "open" | "closed";

const topics = ["cameras", "events", "node", "profiles", "audit"];

/** How long the dashboard's charts reach back. */
const historyWindow = 10 * 60_000;

/**
 * LiveClient keeps the query cache in sync with the node. It remembers the
 * last seq of every topic, so after a reconnection the server replays what
 * was missed, or asks for a resync and the affected queries are refetched.
 * Closing and reopening the browser therefore shows the right state without
 * reloading anything by hand.
 */
export class LiveClient {
  private ws: WebSocket | null = null;
  private seqs = new Map<string, number>();
  private retry = 0;
  private timer: ReturnType<typeof setTimeout> | undefined;
  private stopped = false;
  private status: Status = "closed";
  private listeners = new Set<() => void>();

  constructor(private qc: QueryClient) {}

  start() {
    this.stopped = false;
    this.connect();
  }

  stop() {
    this.stopped = true;
    clearTimeout(this.timer);
    this.ws?.close();
    this.ws = null;
    this.setStatus("closed");
  }

  getStatus = () => this.status;

  subscribeTopic(topic: string) {
    this.send({ op: "subscribe", topics: [topic], since: {} });
  }

  unsubscribeTopic(topic: string) {
    this.seqs.delete(topic);
    this.send({ op: "unsubscribe", topics: [topic] });
  }

  private send(op: object) {
    if (this.ws?.readyState === WebSocket.OPEN) this.ws.send(JSON.stringify(op));
  }

  subscribe = (fn: () => void) => {
    this.listeners.add(fn);
    return () => this.listeners.delete(fn);
  };

  private setStatus(s: Status) {
    this.status = s;
    this.listeners.forEach((fn) => fn());
  }

  private connect() {
    const proto = location.protocol === "https:" ? "wss:" : "ws:";
    const ws = new WebSocket(`${proto}//${location.host}/api/v1/ws`);
    this.ws = ws;
    this.setStatus("connecting");
    ws.onopen = () => {
      this.retry = 0;
      this.setStatus("open");
      ws.send(JSON.stringify({ op: "subscribe", topics: [...topics, ...extraTopics.keys()], since: Object.fromEntries(this.seqs) }));
    };
    ws.onmessage = (ev) => {
      try {
        this.handle(JSON.parse(ev.data as string) as Message);
      } catch {
        // ignore malformed messages
      }
    };
    ws.onclose = () => {
      if (this.ws !== ws) return;
      this.ws = null;
      this.setStatus("closed");
      if (this.stopped) return;
      const wait = Math.min(15_000, 500 * 2 ** this.retry++);
      this.timer = setTimeout(() => this.connect(), wait);
    };
  }

  private handle(m: Message) {
    if (m.type === "subscribed") {
      const prev = this.seqs.get(m.topic);
      this.seqs.set(m.topic, m.seq);
      // Nothing was missed when the seq did not move; a different seq after
      // a reconnection means the node restarted.
      if (prev !== undefined && prev !== m.seq) this.resync(m.topic);
      return;
    }
    if (m.type === "resync") {
      this.seqs.set(m.topic, m.seq);
      this.resync(m.topic);
      return;
    }
    this.seqs.set(m.topic, m.seq);
    switch (m.topic) {
      case "cameras":
        this.onCamera(m);
        break;
      case "events":
        if (m.type === "event") this.onEvent(m.data as EventItem);
        break;
      case "node":
        if (m.type === "metrics") this.onMetrics(m.data as NodeMetrics);
        break;
      case "profiles":
        this.qc.invalidateQueries({ queryKey: keys.profiles });
        break;
      case "audit":
        if (m.type === "entry") this.onAudit(m.data as AuditEntry);
        break;
      default:
        if (m.topic.startsWith("camera:") && m.type === "config") {
          // A client of the emulated API or the panel changed parameters.
          this.qc.invalidateQueries({ queryKey: keys.cameraConfig(m.topic.slice("camera:".length)) });
        }
    }
  }

  private resync(topic: string) {
    if (topic.startsWith("camera:")) {
      this.qc.invalidateQueries({ queryKey: keys.cameraConfig(topic.slice("camera:".length)) });
      return;
    }
    switch (topic) {
      case "cameras":
        this.qc.invalidateQueries({ queryKey: keys.cameras });
        break;
      case "events":
        this.qc.invalidateQueries({ queryKey: ["events"] });
        break;
      case "profiles":
        this.qc.invalidateQueries({ queryKey: keys.profiles });
        break;
      case "audit":
        this.qc.invalidateQueries({ queryKey: ["audit"] });
        break;
    }
  }

  private onMetrics(m: NodeMetrics) {
    this.qc.setQueryData(keys.nodeMetrics, m);
    const sample: NodeSample = {
      at: m.at,
      cpu_percent: m.cpu_percent,
      mem_total: m.mem_total,
      mem_used: m.mem_used,
      net_interface: m.net_interface,
      net_rx_bps: m.net_rx_bps,
      net_tx_bps: m.net_tx_bps,
    };
    const from = Date.parse(m.at) - historyWindow;
    this.qc.setQueryData<NodeSample[]>(keys.nodeHistory, (list) => list && [...list.filter((s) => Date.parse(s.at) > from), sample]);
  }

  /** Puts a new entry on top of every loaded audit list it belongs to. */
  private onAudit(e: AuditEntry) {
    for (const [key, data] of this.qc.getQueriesData<InfiniteData<AuditPage, string>>({ queryKey: ["audit"] })) {
      const filter = key[1] as AuditFilter;
      if (!data?.pages.length || !auditMatches(filter, e)) continue;
      const [first, ...rest] = data.pages;
      if (first.items.some((x) => x.id === e.id)) continue;
      this.qc.setQueryData<InfiniteData<AuditPage, string>>(key, { ...data, pages: [{ ...first, items: [e, ...first.items] }, ...rest] });
    }
  }

  private onCamera(m: Message) {
    switch (m.type) {
      case "status": {
        const s = m.data as { id: string; state: Camera["status"]["state"]; reason: string };
        let found = false;
        this.qc.setQueryData<Camera[]>(keys.cameras, (list) =>
          list?.map((c) => {
            if (c.id !== s.id) return c;
            found = true;
            return { ...c, status: { ...c.status, state: s.state, reason: s.reason } };
          }),
        );
        // Endpoints, PIDs and start times arrive with the full camera.
        if (!found || s.state === "running" || s.state === "stopped") {
          this.qc.invalidateQueries({ queryKey: keys.cameras });
        }
        break;
      }
      case "created":
      case "updated": {
        const cam = m.data as Camera;
        this.qc.setQueryData<Camera[]>(keys.cameras, (list) => {
          if (!list) return list;
          const i = list.findIndex((c) => c.id === cam.id);
          if (i < 0) return [...list, cam].sort((a, b) => a.name.localeCompare(b.name));
          const next = list.slice();
          next[i] = cam;
          return next;
        });
        break;
      }
      case "deleted": {
        const { id } = m.data as { id: string };
        this.qc.setQueryData<Camera[]>(keys.cameras, (list) => list?.filter((c) => c.id !== id));
        break;
      }
    }
  }

  private onEvent(ev: EventItem) {
    const update = (page: EventPage | undefined): EventPage | undefined => {
      if (!page) return page;
      const i = page.items.findIndex((e) => e.id === ev.id);
      if (i >= 0) {
        const items = page.items.slice();
        items[i] = ev;
        return { ...page, items };
      }
      return { ...page, items: [ev, ...page.items].slice(0, 200) };
    };
    this.qc.setQueryData<EventPage>(keys.events(), update);
    this.qc.setQueryData<EventPage>(keys.events(ev.camera_id), update);
    this.qc.setQueryData<EventPage>(keys.failedEvents, (page) => {
      if (!page) return page;
      const i = page.items.findIndex((e) => e.id === ev.id);
      if (i >= 0) {
        const items = page.items.slice();
        items[i] = ev;
        return { ...page, items };
      }
      if (ev.delivery_status !== "failed") return page;
      return { ...page, items: [ev, ...page.items].slice(0, 5) };
    });
  }
}

let client: LiveClient | null = null;

export function startLive(qc: QueryClient) {
  client?.stop();
  client = new LiveClient(qc);
  client.start();
  return client;
}

export function stopLive() {
  client?.stop();
  client = null;
}

// Topics pages asked for, such as camera:<id>, with how many ask. They
// outlive the client, so a reconnection subscribes to them again.
const extraTopics = new Map<string, number>();

function watchTopic(topic: string) {
  const n = (extraTopics.get(topic) ?? 0) + 1;
  extraTopics.set(topic, n);
  if (n === 1) client?.subscribeTopic(topic);
  return () => {
    const left = (extraTopics.get(topic) ?? 1) - 1;
    if (left > 0) {
      extraTopics.set(topic, left);
      return;
    }
    extraTopics.delete(topic);
    client?.unsubscribeTopic(topic);
  };
}

/** Follows the topic of one camera while the component is mounted. */
export function useCameraTopic(id: string) {
  useEffect(() => watchTopic(`camera:${id}`), [id]);
}

const closed = () => "closed" as Status;
const noop = () => () => {};

/** The connection state, for the header indicator. */
export function useLiveStatus(): Status {
  return useSyncExternalStore(client?.subscribe ?? noop, client?.getStatus ?? closed);
}
