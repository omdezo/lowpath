import type {
  Analysis,
  Bootstrap,
  Game,
  Live,
  Mapping,
  NetLocation,
  ProcessInfo,
  RelayInput,
  RelayTest,
  RelayView,
  Route,
  RouteMode,
  ServerList,
  Settings,
} from "./types";

export class ApiError extends Error {
  constructor(
    public kind: string,
    message: string,
    public status: number,
  ) {
    super(message);
  }
}

async function call<T>(method: string, path: string, body?: unknown): Promise<T> {
  const res = await fetch(`/api${path}`, {
    method,
    // The daemon rejects mutating requests without this header (CSRF guard).
    headers: { "X-Lowpath": "1", ...(body !== undefined ? { "Content-Type": "application/json" } : {}) },
    body: body !== undefined ? JSON.stringify(body) : undefined,
  });
  const text = await res.text();
  let data: { error?: string; message?: string } | null = null;
  try {
    data = text ? JSON.parse(text) : null;
  } catch {
    // A proxy or a crashed handler can answer with plain text; keep its words.
    if (!res.ok) throw new ApiError("error", text.trim() || res.statusText, res.status);
    throw new ApiError("bad_response", "The Lowpath service sent an unreadable reply", res.status);
  }
  if (!res.ok) {
    throw new ApiError(data?.error ?? "error", data?.message ?? res.statusText, res.status);
  }
  return data as T;
}

const enc = encodeURIComponent;

export const api = {
  bootstrap: () => call<Bootstrap>("GET", "/bootstrap"),
  mapping: () => call<Mapping>("POST", "/mapping"),
  routes: (id: string) => call<{ routes: Route[] | null; analysis?: Analysis }>("GET", `/games/${enc(id)}/routes`),
  analyze: (id: string) => call<Analysis>("POST", `/games/${enc(id)}/analyze`),
  servers: (id: string) => call<ServerList>("GET", `/games/${enc(id)}/servers`),
  pingServers: (id: string) => call<ServerList>("POST", `/games/${enc(id)}/servers/ping`),
  favorite: (id: string) => call<{ favorites: string[] | null }>("POST", `/games/${enc(id)}/favorite`),
  apply: (game_id: string, mode: RouteMode, region?: string, route_id?: string) =>
    call<Live>("POST", "/apply", { game_id, mode, region, route_id }),
  stop: () => call<Live>("POST", "/stop"),
  saveSettings: (s: Settings) => call<Settings>("PUT", "/settings", s),
  impair: (path_id: string, action: "degrade" | "outage" | "restore") =>
    call<{ ok: boolean }>("POST", "/demo/impair", { path_id, action }),

  relays: () => call<{ relays: RelayView[] | null }>("GET", "/relays"),
  addRelay: (r: RelayInput) => call<RelayView>("POST", "/relays", r),
  deleteRelay: (id: string) => call<{ ok: boolean }>("DELETE", `/relays/${enc(id)}`),
  testRelay: (target: { id: string } | { addr: string; key: string }) => call<RelayTest>("POST", "/relays/test", target),
  processes: () => call<{ processes: ProcessInfo[] | null }>("GET", "/processes"),
  addCustomGame: (name: string, process: string) => call<Game>("POST", "/games/custom", { name, process }),
  deleteCustomGame: (id: string) => call<{ ok: boolean }>("DELETE", `/games/custom/${enc(id)}`),
  elevate: () => call<{ ok: boolean }>("POST", "/elevate"),
  location: (refresh = false) => call<NetLocation>("GET", refresh ? "/location?refresh=1" : "/location"),
};

// Plain-language wording for the error kinds the daemon uses to say "you need
// to do something first", so the toast tells the user what to do next.
export function describeError(e: unknown, fallback: string): { title?: string; msg: string } {
  if (!(e instanceof ApiError)) return { msg: fallback };
  switch (e.kind) {
    case "needs_admin":
      return {
        title: "Administrator rights needed",
        msg: "Route mode captures game traffic with a network driver, which needs administrator rights. Restart Lowpath as administrator, or switch to Proxy mode in Settings → Network.",
      };
    case "no_relays":
      return {
        title: "No relays yet",
        msg: "Add a relay in Settings → Relays, or turn on the loopback test relay to check that capture works.",
      };
    case "capture_failed":
      return { title: "Couldn't capture game traffic", msg: e.message || "The capture driver failed to start." };
    default:
      return { msg: e.message || fallback };
  }
}

export function subscribeLive(onData: (l: Live) => void, onDown: (down: boolean) => void): () => void {
  let es: EventSource | null = null;
  let retry: number | undefined;
  const connect = () => {
    es = new EventSource("/api/live");
    es.onmessage = (e) => {
      let l: Live;
      try {
        l = JSON.parse(e.data);
      } catch {
        return;
      }
      onDown(false);
      onData(l);
    };
    es.onerror = () => {
      onDown(true);
      es?.close();
      retry = window.setTimeout(connect, 1500);
    };
  };
  connect();
  return () => {
    es?.close();
    window.clearTimeout(retry);
  };
}
