import { useCallback, useEffect, useRef, useState } from "react";
import { Globe, Monitor, Router, Server, Waypoints } from "lucide-react";
import { api } from "../api";
import type { GatewaySecond, GatewayStats, GatewayView, RouteSegment, SegmentId } from "../types";
import { fmtMs, pct } from "../util";
import { BloatTest } from "./BloatTest";
import { RouteTrace } from "./RouteTrace";
import { Verdict } from "./Verdict";
import { WiFiCard } from "./WiFiCard";
import "../diag.css";

// Settings → Diagnostics: the verdict with the route split into Home / ISP
// / Lowpath / Game server, the live router pings, and the on-demand tests.
// Nothing here invents a number: a part nobody measured shows "—" and says
// what would measure it.

export function Diagnostics({ onError, wifiTuning }: { onError: (msg: string, title?: string) => void; wifiTuning: boolean }) {
  return (
    <>
      <h3>Diagnostics</h3>
      <div className="muted" style={{ fontSize: 12.5, marginBottom: 6 }}>
        Is the lag on your side of the connection? Keep this tab open on Wi-Fi and again on a cable to compare.
      </div>
      <Verdict renderSegments={(segs) => <RouteSegments segments={segs} />} />
      <GatewayCard />
      <WiFiCard tuningEnabled={wifiTuning} />
      <BloatTest onError={onError} />
      <RouteTrace onError={onError} />
    </>
  );
}

// --- The route, part by part ------------------------------------------------

const SEGMENT_ICON: Record<SegmentId, typeof Router> = { home: Router, isp: Globe, lowpath: Waypoints, server: Server };

const SEGMENT_STATE: Record<RouteSegment["status"], string> = {
  good: "OK",
  warn: "Watch",
  bad: "Problem",
  unknown: "Not measured",
  off: "Not in use",
};

// RouteSegments is the bar under the verdict: this PC, then each part of
// the way to the game server with its own numbers.
export function RouteSegments({ segments }: { segments: RouteSegment[] }) {
  return (
    <div className="segs" aria-label="The route, part by part">
      <div className="segs-end" aria-hidden>
        <Monitor size={16} />
        <span>This PC</span>
      </div>
      <ol className="segs-list">
        {segments.map((s) => {
          const Icon = SEGMENT_ICON[s.id] ?? Globe;
          const nums = s.rtt_ms != null || s.loss_pct != null;
          return (
            <li key={s.id} className={`seg s-${s.status}${s.blamed ? " blamed" : ""}`} data-segment={s.id} data-status={s.status}>
              <div className="seg-bar" aria-hidden />
              <div className="seg-head">
                <Icon size={14} aria-hidden />
                <span className="name">{s.name}</span>
                <span className="state">{s.blamed ? "The problem" : SEGMENT_STATE[s.status]}</span>
              </div>
              <div className="seg-nums num">
                {nums ? (
                  <>
                    <span>
                      {fmtMs(s.rtt_ms)}
                      {s.rtt_ms != null && <small> ms</small>}
                    </span>
                    <span>
                      {s.loss_pct == null ? "—" : pct(s.loss_pct)}
                      {s.loss_pct != null && <small>% loss</small>}
                    </span>
                  </>
                ) : (
                  <span>—</span>
                )}
              </div>
              <div className="seg-sum">{s.summary}</div>
              {s.source && <div className="seg-src faint">{s.source}</div>}
            </li>
          );
        })}
      </ol>
    </div>
  );
}

// --- Live router pings -------------------------------------------------------

// The pinger keeps going while this polls (it stops 15 s after the last
// poll), so the card polls once a second while it is on screen.
const GATEWAY_POLL_MS = 1000;

function linkName(v: GatewayView): string {
  const kind = v.link === "wifi" ? "Wi-Fi" : v.link === "ethernet" ? "Ethernet" : v.link === "other" ? "VPN or other link" : "";
  const parts = [kind, v.interface].filter(Boolean) as string[];
  if (parts.length === 2 && parts[0].toLowerCase() === parts[1].toLowerCase()) parts.pop();
  return parts.join(" · ") || "—";
}

function routerTone(st: GatewayStats): "good" | "warn" | "bad" {
  // The verdict's thresholds: more than 0.5% lost or a p99 above 15 ms.
  if ((!st.limited && st.loss_pct > 0.5 && st.lost >= 2) || (st.p99_ms ?? st.p95_ms ?? 0) > 15) return "bad";
  if (st.spikes > 0 || (!st.limited && st.lost > 0)) return "warn";
  return "good";
}

export function GatewayCard() {
  const [v, setV] = useState<GatewayView | null>(null);
  const [failed, setFailed] = useState(false);
  const timer = useRef<number | undefined>(undefined);
  const alive = useRef(true);

  const poll = useCallback(async () => {
    window.clearTimeout(timer.current);
    try {
      const g = await api.gateway();
      if (!alive.current) return;
      setV(g);
      setFailed(false);
    } catch {
      if (alive.current) setFailed(true);
    }
    if (alive.current) timer.current = window.setTimeout(poll, GATEWAY_POLL_MS);
  }, []);

  useEffect(() => {
    alive.current = true;
    poll();
    return () => {
      alive.current = false;
      window.clearTimeout(timer.current);
    };
  }, [poll]);

  const st = v?.state === "on" ? v.stats : null;
  const tone = st ? routerTone(st) : "none";
  const rate = v?.running && v.rate_hz > 0 ? `${Math.round(v.rate_hz)} a second` : "";

  return (
    <section className="bloat gw" aria-label="Router pings">
      <div className="bloat-head">
        <span className="bloat-ico">
          <Router size={18} />
        </span>
        <div className="txt">
          <div className="t">Router pings</div>
          <div className="d">
            Lowpath pings your router up to 20 times a second while this tab is open and during a boost. Lost pings and slow replies here lag every
            game before its traffic even leaves your home.
          </div>
        </div>
        <span className={`tag gw-live${v?.running ? " on" : ""}`} role="status">
          {v?.running ? <span className="dot" aria-hidden /> : null}
          {v?.running ? `Live · ${rate}` : "Stopped"}
        </span>
      </div>

      {!v && !failed && <div className="bloat-empty muted">Asking the Lowpath service…</div>}
      {failed && <div className="note-warn bloat-note">— The Lowpath service isn't answering, so these pings may be out of date; it will try again.</div>}

      {v && !st && v.message && (
        <div className={`gw-msg ${v.state === "measuring" ? "muted" : "note-warn"}`} data-state={v.state}>
          {v.state === "measuring" ? v.message : `— ${v.message}`}
        </div>
      )}

      {v && st && (
        <>
          <div className={`gw-stats t-${tone}`} data-state={v.state}>
            <Stat k="Median" v={fmtMs(st.p50_ms)} unit="ms" s={`fastest ${fmtMs(st.min_ms)} ms`} />
            <Stat
              k={st.p99_ms != null ? "p99" : "p95"}
              v={fmtMs(st.p99_ms ?? st.p95_ms)}
              unit="ms"
              s={st.p99_ms != null ? `99% of replies; slowest ${fmtMs(st.max_ms)} ms` : "95% of replies; p99 after 100"}
            />
            <Stat
              k="Lost"
              v={st.limited ? "—" : pct(st.loss_pct)}
              unit="%"
              s={st.limited ? "router limits ping replies" : `${st.lost} of ${st.sent}${st.max_loss_run > 1 ? `, ${st.max_loss_run} in a row` : ""}`}
            />
            <Stat k="Spikes" v={String(st.spikes)} unit="" s="replies of 100 ms or more" />
            <Stat k="Jitter" v={fmtMs(st.jitter_ms)} unit="ms" s="change between replies" />
          </div>
          {v.message && (
            <div className="gw-msg note-warn" data-state={v.state}>
              {v.message}
            </div>
          )}
          <GatewayChart series={v.series ?? []} />
        </>
      )}

      {v && v.note && <div className="gw-note muted">{v.note}</div>}

      {v && (v.gateway || v.interface || v.link) && (
        <dl className="bloat-facts">
          <dt>Router</dt>
          <dd className="num">{v.gateway || "—"}</dd>
          <dt>Connection</dt>
          <dd>{linkName(v)}</dd>
          {st && (
            <>
              <dt>Covers</dt>
              <dd>the last {Math.max(1, Math.round(st.window_s))} s</dd>
            </>
          )}
        </dl>
      )}
    </section>
  );
}

function Stat({ k, v, unit, s }: { k: string; v: string; unit: string; s: string }) {
  return (
    <div className="stat">
      <div className="k">{k}</div>
      <div className="v num">
        {v}
        {v !== "—" && unit && <small>{unit}</small>}
      </div>
      <div className="s">{s}</div>
    </div>
  );
}

// The chart's scale: 0 to 100 ms on a square-root axis, so 2 ms and 15 ms
// are both visible next to a 100 ms spike; taller bars are clipped and
// marked.
const CHART_MAX_MS = 100;
const CHART_W = 600;
const CHART_H = 72;
const CHART_SECONDS = 60;

function y(msv: number): number {
  const f = Math.sqrt(Math.min(msv, CHART_MAX_MS) / CHART_MAX_MS);
  return CHART_H - Math.max(1.5, f * (CHART_H - 4));
}

// GatewayChart: one bar per second for the last minute up to the slowest
// reply, a tick at the median; a red cap marks a second with lost pings.
function GatewayChart({ series }: { series: GatewaySecond[] }) {
  if (series.length === 0) return null;
  const last = series[series.length - 1].t;
  const slot = CHART_W / CHART_SECONDS;
  const bars = series.filter((s) => last - s.t < CHART_SECONDS);
  const lostSecs = bars.filter((s) => s.lost > 0).length;
  return (
    <figure className="gw-chart">
      <svg viewBox={`0 0 ${CHART_W} ${CHART_H}`} preserveAspectRatio="none" role="img" aria-label={`Router replies per second over the last minute; ${lostSecs} seconds with lost pings`}>
        {[15, CHART_MAX_MS].map((g) => (
          <line key={g} x1={0} x2={CHART_W} y1={y(g)} y2={y(g)} className="grid" />
        ))}
        {bars.map((s) => {
          const x = (CHART_SECONDS - 1 - (last - s.t)) * slot;
          const w = Math.max(1, slot - 2);
          const lo = s.p50_ms != null ? y(s.p50_ms) : CHART_H;
          const hi = s.max_ms != null ? y(s.max_ms) : CHART_H;
          const spike = (s.max_ms ?? 0) >= CHART_MAX_MS;
          return (
            <g key={s.t}>
              {s.max_ms != null && <rect x={x} y={hi} width={w} height={Math.max(1.5, CHART_H - hi)} className={spike ? "bar spike" : "bar"} />}
              {s.p50_ms != null && <rect x={x} y={lo - 1} width={w} height={2} className="mid" />}
              {s.lost > 0 && <rect x={x} y={0} width={w} height={5} className="lost" />}
            </g>
          );
        })}
      </svg>
      <figcaption className="faint">
        <span>1 min ago</span>
        <span>
          Bars: slowest reply each second, tick: median · lines at 15 and 100 ms · <span className="lost-key">red</span>: lost pings
        </span>
        <span>now</span>
      </figcaption>
    </figure>
  );
}
