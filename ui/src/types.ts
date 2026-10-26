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
  // Where the game's server list comes from before it connects:
  // "sdr" = Valve's Steam Datagram Relay POPs.
  discovery?: string[] | null;
}

// One Valve SDR point of presence with the direct ping from this PC.
// Numbers that weren't measured are null, never 0.
export interface POPView {
  id: string;
  city: string;
  country?: string;
  desc: string;
  lat: number | null;
  lon: number | null;
  geo_fix?: "swapped" | "corrected" | "unknown";
  relays: number;
  ping_ms: number | null;
  loss_pct: number | null;
  sent: number;
  received: number;
  note?: string; // why there is no number
  pinged_at?: string;
}

export interface ServerList {
  source: "valve_sdr" | "";
  app_id?: number;
  state: "unsupported" | "loading" | "ready" | "error";
  message?: string;
  revision?: number;
  fetched_at?: string;
  stale: boolean;
  pinging: boolean;
  pinged_at?: string;
  pops: POPView[] | null;
  no_relay: number;
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
  proto?: "udp" | "tcp";
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
  // Passive bufferbloat detector, during a session only.
  line?: LineState | null;
  // The last boost that ended without the user asking (fail-open), until
  // the next boost.
  failsafe?: Failsafe | null;
  // Other boosters, packet filter drivers and full-tunnel VPNs (null
  // until the first check ends).
  conflicts?: ConflictReport | null;
  // The boost's Wi-Fi scan pause (null: no boost, not on Wi-Fi, or off).
  wifi?: WiFiTuning | null;
}

// --- Wi-Fi (GET /api/diag/wifi) ------------------------------------------

export type WiFiState = "ok" | "no_wifi" | "disconnected" | "unsupported" | "error" | "location";

export interface RadioLink {
  band: string;
  channel: number | null;
  freq_mhz: number;
  width_mhz: number | null;
  rssi_dbm: number | null;
}

// One reading of the Wi-Fi link; null fields weren't reported (notes say why).
export interface WiFiLink {
  at: string;
  interface: string;
  adapter: string;
  rssi_dbm: number | null;
  quality_pct: number | null;
  band: string; // "2.4" | "5" | "6" | ""
  channel: number | null;
  freq_mhz: number | null;
  width_mhz: number | null;
  standard: string;
  generation: string;
  tx_mbps: number | null;
  rx_mbps: number | null;
  mlo: boolean;
  links?: RadioLink[] | null;
  source: string;
  notes?: string[] | null;
}

export interface BSSLoad {
  stations: number;
  busy_pct: number;
  admission_capacity?: number | null;
}

export interface WiFiSurvey {
  at: string;
  load: BSSLoad | null;
  load_note?: string;
  networks: number;
  same_channel: number;
  overlapping: number;
  loudest_neighbor_dbm: number | null;
  freq_mhz: number;
}

export interface WiFiSurveyView {
  state: WiFiState;
  message?: string;
  result: WiFiSurvey | null;
  at: string;
}

export type WiFiTuningState = "applying" | "on" | "partial" | "failed" | "off";

export interface WiFiTuning {
  state: WiFiTuningState;
  scan_paused: boolean;
  streaming: boolean;
  message?: string;
  notes?: string[] | null;
}

export interface WiFiView {
  at: string;
  state: WiFiState;
  message?: string;
  link: WiFiLink | null;
  in_use: boolean;
  route: string;
  survey: WiFiSurveyView | null;
  tuning: WiFiTuning | null;
}

// Other software that competes with Lowpath (internal/compat). "block":
// running and capturing the same packets, route mode won't start next to
// it; "warn": active, may make the boost worse; "info": installed, idle.
export type ConflictLevel = "block" | "warn" | "info";

export interface Conflict {
  id: string;
  name: string;
  kind: "booster" | "packet_tool" | "packet_filter" | "vpn";
  level: ConflictLevel;
  found: string[] | null;
  action: string;
  why: string;
}

export interface ConflictReport {
  at: string;
  conflicts: Conflict[] | null;
  // Checks that couldn't run, with the reason.
  unchecked: string[] | null;
}

// A boost the service ended on its own to keep the game connected
// ("stopped"), or one the last run never finished ("recovered").
export interface Failsafe {
  at: string;
  kind: "stopped" | "recovered";
  game_id: string;
  game: string;
  message: string;
  detail?: string;
}

// Passive detector (internal/diag PassiveState). null numbers are unknown.
export interface LineState {
  active: boolean;
  direction?: "download" | "upload" | "both";
  since?: string;
  watching: boolean;
  reason?: string;
  rtt_ms: number | null;
  baseline_ms: number | null;
  other_down_mbps: number | null;
  other_up_mbps: number | null;
  line_down_mbps: number | null;
  line_up_mbps: number | null;
  line_source?: "test" | "peak";
}

export type BloatPhase = "picking" | "idle" | "download" | "upload" | "recovery";

export interface LatencyStats {
  sent: number;
  lost: number;
  min_ms: number | null;
  p50_ms: number | null;
  p95_ms: number | null;
}

export interface BloatPhaseResult {
  phase: Exclude<BloatPhase, "picking">;
  internet: LatencyStats;
  gateway: LatencyStats | null;
  load_mbps: number | null;
  rx_mbps: number | null;
  tx_mbps: number | null;
  load_error?: string;
}

export type Grade = "A+" | "A" | "B" | "C" | "D" | "F" | "";

export interface BloatResult {
  started_at: string;
  finished_at: string;
  grade: Grade;
  note?: string;
  target: string;
  gateway: string;
  interface: string;
  link: "" | "ethernet" | "wifi" | "other";
  idle_ms: number | null;
  down_increase_ms: number | null;
  up_increase_ms: number | null;
  down_mbps: number | null;
  up_mbps: number | null;
  data_mb: number;
  phases: BloatPhaseResult[] | null;
}

export interface BloatProgress {
  phase: BloatPhase;
  elapsed_s: number;
  total_s: number;
  last_ms: number | null;
  load_mbps: number | null;
  target: string;
}

export interface BloatStatus {
  running: boolean;
  progress: BloatProgress | null;
  result: BloatResult | null;
  error?: string;
}

export type TraceMode = "icmp" | "udp";

// One row of a route trace: a TTL, or (probe "udp", ttl 0) the relay's own
// UDP port. Null numbers were not measured.
export interface TraceHop {
  ttl: number;
  addr: string;
  others?: string[];
  name?: string;
  private: boolean;
  probe: "icmp" | "udp";
  sent: number;
  recv: number;
  loss_pct: number;
  persist_pct: number | null;
  limited: boolean;
  loss_starts: boolean;
  unreachable?: number;
  error?: string;
  best_ms: number | null;
  avg_ms: number | null;
  worst_ms: number | null;
  last_ms: number | null;
  stdev_ms: number | null;
  persist_ms: number | null;
}

export interface TraceResult {
  started_at: string;
  finished_at: string;
  running: boolean;
  stopped: boolean;
  target: string;
  host: string;
  mode: TraceMode;
  end_name?: string;
  rounds: number;
  planned: number;
  interval_s: number;
  max_hops: number;
  reached: boolean;
  hops: TraceHop[] | null;
  end?: TraceHop | null;
  dest_loss_pct: number | null;
  loss_from: number | null;
  threshold_pct: number;
  jump_ttl: number | null;
  jump_ms: number | null;
  verdict: string;
  notes?: string[] | null;
}

export interface TraceTarget {
  id: string;
  label: string;
  addr: string;
  udp: boolean;
  kind: "public" | "relay" | "server";
}

export interface TraceStatus {
  running: boolean;
  progress: TraceResult | null;
  result: TraceResult | null;
  error?: string;
  targets: TraceTarget[] | null;
  defaults: { count: number; interval_s: number };
}

// The verdict engine's answer to "why am I lagging?" (GET /api/diag/verdict).
export type VerdictId = "wifi" | "home" | "line" | "isp" | "relay" | "server" | "optimal" | "clean" | "unknown" | "offline";
export type Confidence = "low" | "medium" | "high";
export type CheckStatus = "found" | "clear" | "unknown";

export interface Verdict {
  id: VerdictId;
  segment?: "home" | "isp" | "lowpath" | "server";
  title: string;
  detail: string;
  advice?: string;
  confidence: Confidence;
  lowpath_helps: boolean;
  facts?: string[] | null;
}

export interface VerdictCheck {
  rule: "home" | "line" | "isp" | "server" | "optimal";
  name: string;
  status: CheckStatus;
  summary: string;
  facts?: string[] | null;
}

export interface Diagnosis {
  at: string;
  verdict: Verdict;
  checks: VerdictCheck[] | null;
  link: string;
  interface: string;
  next?: { id: "bloat" | "trace" | "measure"; text: string }[] | null;
  // The route from this PC to the game server, part by part.
  segments?: RouteSegment[] | null;
}

export type SegmentId = "home" | "isp" | "lowpath" | "server";
export type SegmentStatus = "good" | "warn" | "bad" | "unknown" | "off";

export interface RouteSegment {
  id: SegmentId;
  name: string;
  status: SegmentStatus;
  rtt_ms: number | null;
  loss_pct: number | null;
  source?: string;
  summary: string;
  blamed: boolean;
}

// GET /api/diag/gateway: the router pinger.
export type GatewayState =
  | "idle"
  | "measuring"
  | "on"
  | "paused"
  | "no_gateway"
  | "offline"
  | "no_reply"
  | "no_permission"
  | "cant_send"
  | "unsupported";

export interface GatewayStats {
  window_s: number;
  sent: number;
  lost: number;
  loss_pct: number;
  p50_ms: number | null;
  p95_ms: number | null;
  p99_ms: number | null;
  min_ms: number | null;
  max_ms: number | null;
  jitter_ms: number | null;
  spikes: number;
  max_loss_run: number;
  limited: boolean;
}

export interface GatewaySecond {
  t: number;
  sent: number;
  lost: number;
  p50_ms: number | null;
  max_ms: number | null;
}

export interface GatewayView {
  state: GatewayState;
  message?: string;
  note?: string;
  running: boolean;
  rate_hz: number;
  gateway?: string;
  interface?: string;
  link?: string;
  since: string;
  stats: GatewayStats | null;
  series: GatewaySecond[] | null;
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
  // Pause the Wi-Fi adapter's background scans while boosting over Wi-Fi.
  wifi_tuning?: boolean;
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

// The registry's answer for a public address (internal/diag NetworkInfo).
export interface NetworkInfo {
  asn?: number;
  asns?: number[];
  holder?: string;
  as_name?: string;
  isp?: string;
  prefix?: string;
  country?: string;
  city?: string;
  lat?: number;
  lon?: number;
  partial?: string;
}

// GET /api/location: the public address the internet sees and the network
// behind it (internal/diag). Empty fields are unknown.
export interface Endpoint {
  ip: string;
  source: string;
  asn?: number;
  asns?: number[];
  holder?: string;
  as_name?: string;
  isp?: string;
  prefix?: string;
  country?: string;
  city?: string;
  partial?: string;
  lookup_error?: string;
}

export interface NetLocation {
  state: "ok" | "partial" | "offline";
  message?: string;
  at: string;
  ipv4: Endpoint | null;
  ipv6: Endpoint | null;
  ipv6_note?: string;
  // Set when the ISP is unknown only because the registry was slow or
  // busy: asking again (without refresh) after this many seconds usually
  // gets it.
  retry_after_s?: number;
}

// GET /api/games/{id}/current-server: the server the game is talking to
// (internal/app/current.go). Unknown numbers are null.
export interface GeoCheck {
  state: "ok" | "too_far" | "unknown";
  distance_km: number | null;
  max_km: number | null;
  note?: string;
}

export interface CurrentServerInfo {
  addr: string;
  proto?: "udp" | "tcp";
  pkts_up: number | null;
  pkts_down: number | null;
  last_seen: string | null;
  network: NetworkInfo | null;
  network_note?: string;
  looking_up: boolean;
  lan: boolean;
  geo: GeoCheck;
  ping_ms: number | null;
  jitter_ms: number | null;
  loss_pct: number | null;
  sent: number;
  received: number;
  ping_note?: string;
}

export interface CurrentServer {
  state: "live" | "last" | "none";
  source?: "boost" | "traffic" | "history";
  message?: string;
  watch: { state: "on" | "idle" | "needs_admin" | "unsupported" | "error"; message?: string };
  server: CurrentServerInfo | null;
  others: { addr: string; proto: string; pkts_up: number; pkts_down: number }[];
  you: { city?: string; country?: string } | null;
}
