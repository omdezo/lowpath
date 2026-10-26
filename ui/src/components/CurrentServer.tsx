import { useCallback, useEffect, useRef, useState } from "react";
import { AlertTriangle, Loader2, MapPin, Radio } from "lucide-react";
import { api } from "../api";
import type { CurrentServer as CurrentServerView, CurrentServerInfo } from "../types";
import { fmtMs, pct } from "../util";

// The card polls while it is on screen; that is also what keeps the server
// pinged (the service stops pinging a server nobody looks at).
const POLL_MS = 2000;
const RETRY_MS = 5000;

const SOURCE: Record<string, string> = {
  boost: "Seen in the traffic Lowpath is carrying.",
  traffic: "Seen in the game's own traffic, without routing it.",
  history: "The server it used last time. It may pick another one next match.",
};

// Same thresholds as the Valve servers card and the route quality.
function pingQ(v: number): "good" | "warn" | "bad" {
  if (v >= 160) return "bad";
  if (v >= 90) return "warn";
  return "good";
}

// CurrentServer names the server a game is talking to, for every game:
// where it is (with the city checked against the ping), whose network it
// is on, and how the direct route to it behaves right now.
export function CurrentServer({ gameId, gameName }: { gameId: string; gameName: string }) {
  const [view, setView] = useState<CurrentServerView | null>(null);
  const [failed, setFailed] = useState(false);
  const timer = useRef<number | undefined>(undefined);
  const gen = useRef(0);

  const load = useCallback(async () => {
    window.clearTimeout(timer.current);
    const g = gen.current;
    try {
      const v = await api.currentServer(gameId);
      if (g !== gen.current) return;
      setView(v);
      setFailed(false);
      timer.current = window.setTimeout(load, POLL_MS);
    } catch {
      if (g !== gen.current) return;
      setFailed(true);
      timer.current = window.setTimeout(load, RETRY_MS);
    }
  }, [gameId]);

  useEffect(() => {
    gen.current++;
    setView(null);
    load();
    return () => {
      gen.current++;
      window.clearTimeout(timer.current);
    };
  }, [load]);

  const sv = view?.server ?? null;
  const live = view?.state === "live";
  const watchNote = view && view.watch.state !== "on" && view.watch.state !== "idle" ? view.watch.message : undefined;

  return (
    <section className="card pops cur" aria-label="Current server">
      <div className="pops-head">
        <div className="pops-titles">
          <span className="section-label">
            Current server
            {live && (
              <span className="cur-badge" aria-label="live">
                <span className="live-dot" aria-hidden="true" /> Live
              </span>
            )}
            {view?.state === "last" && <span className="cur-badge cur-badge-last">Last time</span>}
          </span>
          <div className="pops-sub">{view?.source ? SOURCE[view.source] : `The server ${gameName} is connected to, learned from its traffic.`}</div>
        </div>
      </div>

      {!view && !failed && (
        <div className="cur-body" aria-busy="true">
          <span className="skeleton" style={{ width: 180 }} />
        </div>
      )}
      {failed && !view && <div className="pops-msg faint">Can't reach the Lowpath service right now; trying again.</div>}

      {view?.state === "none" && <div className="pops-msg faint">{view.message}</div>}

      {sv && <ServerDetails sv={sv} you={view?.you?.city} />}

      {view && view.others.length > 0 && (
        <div className="cur-others faint">
          Also talking to{" "}
          {view.others.map((o, i) => (
            <span key={o.addr}>
              {i > 0 && ", "}
              <span className="num">{o.addr}</span> {o.proto.toUpperCase()}
            </span>
          ))}
        </div>
      )}

      {watchNote && (
        <div className="cur-note faint" role="note">
          <Radio size={13} aria-hidden="true" /> {watchNote}
        </div>
      )}
    </section>
  );
}

function ServerDetails({ sv, you }: { sv: CurrentServerInfo; you?: string }) {
  const n = sv.network;
  const place = n?.city ? `${n.city}${n.country ? `, ${n.country}` : ""}` : n?.country ? n.country : null;
  const tooFar = sv.geo.state === "too_far";
  const asLine = n?.asn ? `AS${n.asn}${n.isp ? ` · ${n.isp}` : n.as_name ? ` · ${n.as_name}` : ""}` : null;
  const pingKnown = sv.ping_ms != null;
  return (
    <div className="cur-body">
      <div className="cur-place">
        <MapPin size={16} className="pop-icon" aria-hidden="true" />
        <div style={{ minWidth: 0 }}>
          <div className="cur-city">
            {place ? (
              <span className={tooFar ? "cur-struck" : undefined}>{place}</span>
            ) : sv.looking_up ? (
              <span className="faint">
                <Loader2 size={13} className="spin" /> Looking up…
              </span>
            ) : (
              <span className="faint">Location unknown</span>
            )}
          </div>
          {sv.geo.note && (
            <div className={`cur-geo ${tooFar ? "q-warn" : "faint"}`}>
              {tooFar && <AlertTriangle size={12} aria-hidden="true" />} {sv.geo.note}
              {you && sv.geo.distance_km != null ? ` (from ${you})` : ""}
            </div>
          )}
        </div>
      </div>

      <dl className="cur-facts">
        <div>
          <dt>Address</dt>
          <dd className="num">
            {sv.addr}
            {sv.proto ? ` · ${sv.proto.toUpperCase()}` : ""}
          </dd>
        </div>
        <div>
          <dt>Network</dt>
          <dd>{asLine ?? <span className="faint">{sv.looking_up ? "Looking up…" : (sv.network_note ?? "—")}</span>}</dd>
        </div>
        {n?.prefix && (
          <div>
            <dt>Prefix</dt>
            <dd className="num">{n.prefix}</dd>
          </div>
        )}
      </dl>

      <div className="cur-metrics" title={pingKnown ? "Direct ICMP ping from this PC, last 30 s" : sv.ping_note}>
        <Metric label="Ping" value={pingKnown ? fmtMs(sv.ping_ms) : "—"} unit="ms" q={pingKnown ? pingQ(sv.ping_ms!) : undefined} />
        <Metric label="Jitter" value={sv.jitter_ms != null ? fmtMs(sv.jitter_ms) : "—"} unit="ms" q={sv.jitter_ms != null && sv.jitter_ms >= 12 ? "warn" : undefined} />
        <Metric label="Loss" value={sv.loss_pct != null ? pct(sv.loss_pct) : "—"} unit="%" q={sv.loss_pct != null && sv.loss_pct > 0 ? "warn" : undefined} />
      </div>
      {!pingKnown && sv.ping_note && <div className="cur-geo faint">{sv.ping_note}</div>}
      {pingKnown && <div className="pops-foot faint">Direct ICMP ping from this PC, last {sv.sent} pings · network from RIPEstat</div>}
    </div>
  );
}

function Metric({ label, value, unit, q }: { label: string; value: string; unit: string; q?: string }) {
  const known = value !== "—";
  return (
    <div className="cur-metric">
      <div className="k">{label}</div>
      <div className={`v num ${known ? (q ? `q-${q}` : "") : "faint"}`}>
        {value}
        {known && <small>{unit}</small>}
      </div>
    </div>
  );
}
