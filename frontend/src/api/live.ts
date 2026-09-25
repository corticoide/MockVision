import type { QueryClient } from "@tanstack/react-query";
import { useSyncExternalStore } from "react";
import type { Camera, EventItem, EventPage, NodeMetrics } from "./client";
import { keys } from "./queries";

/** A message of the WebSocket: every topic numbers its messages. */
interface Message {
  topic: string;
  seq: number;
  type: string;
  at: number;
  data?: unknown;
}

type Status = "connecting" | "open" | "closed";

const topics = ["cameras", "events", "node", "profiles"];

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
      ws.send(JSON.stringify({ op: "subscribe", topics, since: Object.fromEntries(this.seqs) }));
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
        if (m.type === "metrics") this.qc.setQueryData(keys.nodeMetrics, m.data as NodeMetrics);
        break;
      case "profiles":
        this.qc.invalidateQueries({ queryKey: keys.profiles });
        break;
    }
  }

  private resync(topic: string) {
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

const closed = () => "closed" as Status;
const noop = () => () => {};

/** The connection state, for the header indicator. */
export function useLiveStatus(): Status {
  return useSyncExternalStore(client?.subscribe ?? noop, client?.getStatus ?? closed);
}
