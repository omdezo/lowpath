import { ArrowDown, ArrowUp, Radar } from "lucide-react";
import type { CaptureStats, GameServer } from "../types";
import { since } from "../util";

interface Props {
  gameName: string;
  processes: string[];
  servers: GameServer[];
  capture: CaptureStats | null;
  now: number;
}

// Route mode, while optimizing: either "launch the game now" or the game
// servers the captured traffic is going to, plus the capture counters that
// prove packets are really being intercepted and put back.
export function TrafficCard({ gameName, processes, servers, capture, now }: Props) {
  const pids = capture?.game_pids ?? [];
  const sorted = [...servers].sort((a, b) => Number(b.primary) - Number(a.primary) || b.pkts_up - a.pkts_up);

  return (
    <div className="card traffic" aria-live="polite">
      {sorted.length === 0 ? (
        <div className="waiting">
          <span className="radar radar-sm" aria-hidden="true">
            <Radar size={20} />
          </span>
          <div>
            <div className="t">Waiting for game traffic — launch the game now</div>
            <div className="d">
              {pids.length > 0
                ? `${gameName} is running (PID ${pids.join(", ")}). Join a match or open the server list; its servers show up here as soon as it sends packets.`
                : `Lowpath captures ${processes.length ? processes.join(", ") : gameName} automatically once it starts sending packets. Nothing to change in the game's settings.`}
            </div>
          </div>
        </div>
      ) : (
        <>
          <div className="traffic-head">
            <span className="section-label">Game servers</span>
            {pids.length > 0 && <span className="hint num">PID {pids.join(", ")}</span>}
          </div>
          <div className="server-list" role="list" aria-label="Game servers">
            {sorted.map((s) => (
              <div className="server-row" role="listitem" key={s.addr}>
                <span className="addr num">{s.addr}</span>
                {s.primary && <span className="tag tag-accent">Primary</span>}
                <span className="spacer" />
                <span className="num pk" title="Packets sent to this server">
                  <ArrowUp size={12} /> {s.pkts_up.toLocaleString()}
                </span>
                <span className="num pk" title="Packets received from this server">
                  <ArrowDown size={12} /> {s.pkts_down.toLocaleString()}
                </span>
                <span className="faint num seen">{seen(s.last_seen, now)}</span>
              </div>
            ))}
          </div>
        </>
      )}
      {capture && (
        <div className="counters">
          <Counter k="Captured" v={capture.captured} title="Game packets intercepted on their way out" />
          <Counter k="Sent direct" v={capture.passed} title="Game packets let through unchanged (routes down with the kill switch off)" />
          <Counter k="Injected" v={capture.injected} title="Replies delivered back into the game" />
          <Counter k="Errors" v={capture.errors} bad={capture.errors > 0} />
          {(capture.route_repairs ?? 0) > 0 && (
            <Counter
              k="Route fixes"
              v={capture.route_repairs ?? 0}
              bad
              title="Times another program (a network manager, VPN or firewall) removed Lowpath's routing (its rule or firewall table) and Lowpath put it back; the game's packets took your normal connection for up to a second each time"
            />
          )}
        </div>
      )}
      {capture?.last_error && (
        <div className="capture-error q-bad" role="alert">
          Last capture error: {capture.last_error}
        </div>
      )}
    </div>
  );
}

function Counter({ k, v, title, bad }: { k: string; v: number; title?: string; bad?: boolean }) {
  return (
    <div className="counter" title={title}>
      <div className="k">{k}</div>
      <div className={`v num ${bad ? "q-bad" : ""}`}>{(v ?? 0).toLocaleString()}</div>
    </div>
  );
}

function seen(iso: string, now: number): string {
  const t = new Date(iso).getTime();
  if (!Number.isFinite(t)) return "";
  return now - t < 2000 ? "now" : `${since(iso, now)} ago`;
}
