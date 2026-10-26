// Mirrors the JSON returned by Lowpath (internal/app, internal/engine).
// Fields added for the Windows MVP are optional so the UI keeps working
// against a daemon that doesn't send them yet.

export interface Stats {
  rtt_ms: number;
  loss_pct: number;
  jitter_ms: number;
  up: boolean;
  samples: number;
  // Relay → game server leg already included in rtt_ms; 0 when unknown.
  server_ms?: number;
}

export interface Game {
  id: string;
  name: string;
  genre: string;
  color: string;
  smart: boolean;
  favorite: boolean;
  installed: boolean;
  running: boolean;
  routable: boolean;
  custom?: boolean;
  processes?: string[] | null;
  steam_app?: number; // Steam app id: the service has library artwork for it
}

export interface Route {
  id: string;
  label: string;
  region: string;
  continent: string;
}

export interface PathStatus extends Route, Stats {
  active: boolean;
}

export interface EngineEvent {
  ts: string;
  kind: "applied" | "switch" | "degraded" | "fallback" | "recovered";
  msg: string;
}

// A game server the captured game is talking to (route mode).
export interface GameServer {
  addr: string;
  pkts_up: number;
  pkts_down: number;
  last_seen: string;
  primary: boolean;
}

export interface Snapshot {
  mode?: "proxy" | "capture";
  listen: string;
  before: Stats;
  before_known?: boolean;
  after: Stats;
  server_leg_ms?: number;
  server_leg_known?: boolean;
  servers?: GameServer[] | null;
  paths: PathStatus[] | null;
  copies: number;
  flows: number;
  pkts_up: number;
  pkts_down: number;
  dups_dropped: number;
  fallback: boolean;
  events: EngineEvent[] | null;
}

export type RouteMode = "smart" | "region" | "specific";
export type CaptureMode = "route" | "proxy";

export interface Session {
  game_id: string;
  mode: RouteMode;
  region?: string;
  route_id?: string;
  started_at: string;
  listen: string;
  capture_mode?: CaptureMode;
}

export type Status = "idle" | "analyzing" | "applying" | "optimizing";

export interface CaptureStats {
  mode: "route";
  captured: number;
  passed: number;
  injected: number;
  errors: number;
  game_pids: number[] | null;
  last_error: string;
}

export interface Live {
  status: Status;
  session: Session | null;
  snapshot: Snapshot | null;
  capture?: CaptureStats | null;
  running: string[] | null;
}

export interface Settings {
  dup_level: 0 | 1 | 2 | 4;
  auto_switch: boolean;
  kill_switch: boolean;
  auto_analyze: boolean;
  stop_on_game_exit: boolean;
  theme: "dark" | "light";
  launch_on_boot: boolean;
  minimize_to_tray: boolean;
  notifications: boolean;
  capture_mode?: CaptureMode;
  loopback_relay?: boolean;
}

export interface Analysis {
  at: string;
  direct: Stats;
  // false when no game server is known yet or it ignores ping.
  direct_known?: boolean;
  paths: Record<string, Stats>;
}

export interface Mapping {
  at: string;
  relays: number;
  best: string;
  best_ms: number;
}

export interface Platform {
  os: string;
  admin: boolean;
  /** The app can restart itself with the rights route mode needs. */
  can_elevate: boolean;
  capture: { backend: string; available: boolean; reason: string };
}

export interface RelayView {
  id: string;
  label: string;
  region: string;
  continent: string;
  addr: string;
  // Id of the egress relay this relay's traffic leaves from.
  egress: string;
  has_key: boolean;
  builtin: boolean;
}

export interface RelayInput {
  label: string;
  region: string;
  continent: string;
  addr: string;
  key: string;
  egress: string;
}

export interface RelayTest {
  ok: boolean;
  rtt_ms: number;
  loss_pct: number;
  message: string;
}

export interface ProcessInfo {
  pid: number;
  name: string;
  udp_ports: number[] | null;
}

export interface Bootstrap {
  version: string;
  demo: boolean;
  platform?: Platform | null;
  relays?: RelayView[] | null;
  settings: Settings;
  games: Game[];
  recent: string[] | null;
  mapping: Mapping | null;
  live: Live;
}
