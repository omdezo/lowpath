import { useCallback, useEffect, useRef, useState } from "react";
import { AlertTriangle, Loader2, RotateCw, Server } from "lucide-react";
import { api } from "../api";
import type { POPView, ServerList } from "../types";
import { ms, pct, since } from "../util";

const SHOWN = 6; // rows before "Show all"
const POLL_MS = 1200; // while the list downloads or a ping round runs
// After a failed download: the service retries on its own backoff, but only
// when asked, so keep asking or "Trying again in 30 s" would never happen.
const RETRY_POLL_MS = 10_000;
// While the page stays open: lets the hourly list refresh and the 5-minute
// re-ping happen without leaving and reopening the game.
const IDLE_POLL_MS = 60_000;

// Ping colors match the route quality thresholds (util.ts quality()).
function pingQ(v: number): "good" | "warn" | "bad" {
  if (v >= 160) return "bad";
  if (v >= 90) return "warn";
  return "good";
}

const GEO_NOTE: Record<string, string> = {
  swapped: "Valve's coordinates had latitude and longitude swapped; fixed.",
  corrected: "Valve's coordinates pointed somewhere else; replaced with the city's.",
};

// ValveServers lists the Steam Datagram Relay points of presence a Valve
// game plays through, with a direct ping from this PC to each, before the
// game is even started. The list comes from Steam and is refreshed in the
// background; nothing here waits on the network.
export function ValveServers({ gameId, gameName, now }: { gameId: string; gameName: string; now: number }) {
  const [list, setList] = useState<ServerList | null>(null);
  const [failed, setFailed] = useState(false);
  const [all, setAll] = useState(false);
  const timer = useRef<number | undefined>(undefined);
  // Bumped when the game changes or the card unmounts: a reply that comes
  // back after that belongs to the old game and is dropped.
  const gen = useRef(0);

  const load = useCallback(
    async (ping = false) => {
      window.clearTimeout(timer.current);
      const g = gen.current;
      try {
        const l = ping ? await api.pingServers(gameId) : await api.servers(gameId);
        if (g !== gen.current) return;
        setList(l);
        setFailed(false);
        if (l.state === "unsupported") return;
        const wait = l.state === "loading" || l.pinging ? POLL_MS : l.state === "error" ? RETRY_POLL_MS : IDLE_POLL_MS;
        timer.current = window.setTimeout(() => load(), wait);
      } catch {
        if (g !== gen.current) return;
        // The local service is restarting or busy: say so, try again later.
        setFailed(true);
        timer.current = window.setTimeout(() => load(), 5000);
      }
    },
    [gameId],
  );

  useEffect(() => {
    gen.current++;
    setList(null);
    setAll(false);
    load();
    return () => {
      gen.current++;
      window.clearTimeout(timer.current);
    };
  }, [load]);

  if (list?.state === "unsupported") return null;

  const pops = list?.pops ?? [];
  const rows = all ? pops : pops.slice(0, SHOWN);
  const loading = !list || list.state === "loading";
  const pinging = !!list?.pinging;
  // No list after a failed download: the button asks for it again now.
  const retry = list?.state === "error";

  return (
    <section className="card pops" aria-label="Valve servers">
      <div className="pops-head">
        <div className="pops-titles">
          <span className="section-label">Valve servers</span>
          <div className="pops-sub">
            Direct ping from this PC to the relays {gameName} plays through. Valve routes the match from there, so Lowpath can only improve the way to the nearest one.
          </div>
        </div>
        <button className="btn btn-sm" onClick={() => load(true)} disabled={loading || pinging || (list?.state !== "ready" && !retry)}>
          {pinging ? <Loader2 size={14} className="spin" /> : <RotateCw size={14} />}
          {pinging ? "Pinging…" : retry ? "Try again" : "Ping again"}
        </button>
      </div>

      {failed && !list && <div className="pops-msg faint">Can't reach the Lowpath service right now; trying again.</div>}

      {list?.state === "error" && (
        <div className="note-warn" role="status">
          <AlertTriangle size={14} /> {list.message}
        </div>
      )}

      {list?.state === "ready" && list.stale && list.message && (
        <div className="note-warn" role="status">
          <AlertTriangle size={14} /> {list.message}
        </div>
      )}

      {loading && !failed && (
        <div className="pop-list" aria-busy="true">
          {[0, 1, 2].map((i) => (
            <div className="pop-row" key={i}>
              <span className="pop-name">
                <span className="skeleton" style={{ width: 120 }} />
              </span>
              <span className="metric">
                <span className="skeleton" />
              </span>
              <span className="metric">
                <span className="skeleton" style={{ width: 30 }} />
              </span>
            </div>
          ))}
          <div className="pops-msg faint">Getting Valve's server list from Steam…</div>
        </div>
      )}

      {pops.length > 0 && (
        <>
          <div className="pop-row pop-row-head section-label" aria-hidden="true">
            <span>Location</span>
            <span style={{ textAlign: "right" }}>Ping</span>
            <span style={{ textAlign: "right" }}>Loss</span>
          </div>
          <div className="pop-list" role="list" aria-label="Valve servers">
            {rows.map((p) => (
              <POPRow key={p.id} pop={p} pinging={pinging} />
            ))}
          </div>
          {pops.length > SHOWN && (
            <button className="link pops-more" onClick={() => setAll((a) => !a)} aria-expanded={all}>
              {all ? "Show fewer" : `Show all ${pops.length}`}
            </button>
          )}
        </>
      )}

      {list?.state === "ready" && pops.length === 0 && <div className="pops-msg faint">Valve's list has no relay addresses to ping for {gameName}.</div>}

      {list?.state === "ready" && (
        <div className="pops-foot faint">
          From Steam{list.fetched_at ? `, ${list.stale ? "saved" : "updated"} ${since(list.fetched_at, now)} ago` : ""} · ICMP ping, best of up to 4
          {list.no_relay > 0 ? ` · ${list.no_relay} more data centers have no public relay address` : ""}
        </div>
      )}
    </section>
  );
}

function POPRow({ pop, pinging }: { pop: POPView; pinging: boolean }) {
  const measured = pop.ping_ms != null;
  const pending = !measured && !pop.pinged_at;
  const loss = pop.loss_pct;
  const sub = [pop.id, pop.country].filter(Boolean).join(" · ");
  const geo = pop.geo_fix ? GEO_NOTE[pop.geo_fix] : undefined;
  return (
    <div className="pop-row" role="listitem" title={geo}>
      <span className="pop-name">
        <Server size={14} className="pop-icon" aria-hidden="true" />
        <span style={{ minWidth: 0 }}>
          <span className="title">{pop.city}</span>
          <span className="desc">
            {sub}
            {!measured && !pending && pop.note ? ` · ${pop.note}` : ""}
          </span>
        </span>
      </span>
      <span className="metric num">
        {measured ? (
          <>
            <span className={`q-${pingQ(pop.ping_ms!)}`}>{ms(pop.ping_ms!)}</span>
            <small>ms</small>
          </>
        ) : pending && pinging ? (
          <span className="skeleton" />
        ) : (
          <span className="faint" title={pop.note ?? (pending ? "Not measured yet" : undefined)}>
            —
          </span>
        )}
      </span>
      <span className="metric num">
        {measured && loss != null ? (
          <>
            <span className={loss > 0 ? "q-warn" : ""}>{pct(loss)}</span>
            <small>%</small>
          </>
        ) : (
          <span className="faint">—</span>
        )}
      </span>
    </div>
  );
}
