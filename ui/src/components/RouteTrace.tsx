import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Download, Loader2, Play, Route, Square } from "lucide-react";
import { api, describeError } from "../api";
import type { TraceHop, TraceMode, TraceResult, TraceStatus, TraceTarget } from "../types";
import { fmtMs, pct } from "../util";

// The route trace (MTR): every router between this PC and a target, with
// loss and delay at each, measured many times. Loss only counts when it
// carries on to the destination; routers that skip answering trace probes
// are marked, not blamed. Unknown numbers show "—" with the reason.

const POLL_MS = 1000;
const OTHER = "__other";

interface Props {
  onError: (msg: string, title?: string) => void;
}

export function RouteTrace({ onError }: Props) {
  const [st, setSt] = useState<TraceStatus | null>(null);
  const [busy, setBusy] = useState(false);
  const [targetId, setTargetId] = useState("");
  const [host, setHost] = useState("");
  const [mode, setMode] = useState<TraceMode>("icmp");
  const timer = useRef<number | undefined>(undefined);
  const alive = useRef(true);

  const poll = useCallback(async () => {
    window.clearTimeout(timer.current);
    try {
      const s = await api.trace();
      if (!alive.current) return;
      setSt(s);
      if (s.running) timer.current = window.setTimeout(poll, POLL_MS);
    } catch {
      if (alive.current) timer.current = window.setTimeout(poll, 3000);
    }
  }, []);

  useEffect(() => {
    alive.current = true;
    poll();
    return () => {
      alive.current = false;
      window.clearTimeout(timer.current);
    };
  }, [poll]);

  const targets = useMemo(() => st?.targets ?? [], [st?.targets]);
  // Default to the first relay (the route games take with Lowpath), else
  // whatever comes first.
  const chosen: TraceTarget | undefined = targets.find((t) => t.id === targetId) ?? (targetId === OTHER ? undefined : targets[0]);
  const selectValue = targetId === OTHER ? OTHER : (chosen?.id ?? "");
  const udpOk = !!chosen?.udp && targetId !== OTHER;
  const effMode: TraceMode = udpOk ? mode : "icmp";

  const start = async () => {
    const target = targetId === OTHER ? { host: host.trim() } : chosen ? { target_id: chosen.id } : null;
    if (!target) return;
    setBusy(true);
    try {
      setSt(await api.startTrace(target, effMode));
      timer.current = window.setTimeout(poll, POLL_MS);
    } catch (e) {
      const d = describeError(e, "Couldn't start the route trace");
      onError(d.msg, d.title);
    } finally {
      setBusy(false);
    }
  };

  const stop = async () => {
    setBusy(true);
    try {
      setSt(await api.stopTrace());
    } catch (e) {
      onError(describeError(e, "Couldn't stop the route trace").msg);
    } finally {
      setBusy(false);
    }
  };

  const running = !!st?.running;
  const shown = running ? st?.progress : st?.result;
  const minutes = st ? duration(st.defaults.count * st.defaults.interval_s) : "";
  const canStart = !!st && (targetId === OTHER ? host.trim() !== "" : !!chosen);

  return (
    <section className="bloat trace" aria-label="Route trace">
      <div className="bloat-head">
        <span className="bloat-ico">
          <Route size={18} />
        </span>
        <div className="txt">
          <div className="t">Route trace (MTR)</div>
          <div className="d">
            Finds every router between this PC and the target and measures loss and delay at each, {st?.defaults.count ?? 200} times. Loss only
            counts when it carries on to the destination: a router that skips answering trace probes isn't losing your traffic. Takes {minutes || "a few minutes"}.
          </div>
        </div>
      </div>

      <div className="trace-controls">
        <label className="trace-field">
          <span className="section-label">Target</span>
          <select className="input" value={selectValue} disabled={running || !st} onChange={(e) => setTargetId(e.target.value)} aria-label="Trace target">
            {targets.map((t) => (
              <option key={t.id} value={t.id}>
                {targetLabel(t)}
              </option>
            ))}
            <option value={OTHER}>Other host or IP…</option>
          </select>
        </label>
        {targetId === OTHER && (
          <label className="trace-field trace-host">
            <span className="section-label">Host</span>
            <input
              className="input num"
              placeholder="game.example.com or 203.0.113.10"
              value={host}
              disabled={running}
              spellCheck={false}
              autoComplete="off"
              onChange={(e) => setHost(e.target.value)}
              onKeyDown={(e) => e.key === "Enter" && canStart && !running && start()}
              aria-label="Host to trace"
            />
          </label>
        )}
        <div className="trace-field">
          <span className="section-label" id="trace-mode-label">
            Probes
          </span>
          <div className="segmented" role="radiogroup" aria-labelledby="trace-mode-label">
            <button role="radio" aria-checked={effMode === "icmp"} disabled={running} onClick={() => setMode("icmp")}>
              ICMP
            </button>
            <button
              role="radio"
              aria-checked={effMode === "udp"}
              disabled={running || !udpOk}
              onClick={() => setMode("udp")}
              title={udpOk ? "Measure the relay over UDP on its own port, like game traffic" : "UDP needs one of your relays as the target"}
            >
              UDP
            </button>
          </div>
        </div>
        <div className="trace-go">
          {running ? (
            <button className="btn" onClick={stop} disabled={busy}>
              <Square size={14} /> Stop
            </button>
          ) : (
            <button className="btn btn-primary" onClick={start} disabled={busy || !canStart}>
              {busy ? <Loader2 size={14} className="spin" /> : <Play size={14} />} {st?.result ? "Trace again" : "Start trace"}
            </button>
          )}
        </div>
      </div>
      {effMode === "udp" && !running && (
        <div className="trace-hint faint">Routers are found with ICMP; the last row measures the relay's UDP port, the way game packets travel.</div>
      )}

      {running && shown && <Progress r={shown} />}
      {!running && st?.error && <div className="note-warn bloat-note">{st.error}</div>}
      {!running && shown && <Verdict r={shown} />}
      {shown && (shown.hops?.length ?? 0) + (shown.end ? 1 : 0) > 0 && <HopTable r={shown} />}
      {!running && shown && <Footer r={shown} />}
      {!running && !shown && st && !st.error && (
        <div className="bloat-empty muted">No trace yet. Trace to a relay to see where loss on the way to it starts.</div>
      )}
    </section>
  );
}

function targetLabel(t: TraceTarget): string {
  const kind = t.kind === "relay" ? "relay" : t.kind === "server" ? "last server" : "";
  return `${t.label} · ${t.addr}${kind && !t.label.toLowerCase().includes(kind) ? ` (${kind})` : ""}`;
}

function duration(s: number): string {
  if (!s || s <= 0) return "";
  if (s < 90) return `about ${Math.round(s)} seconds`;
  const m = Math.floor(s / 60);
  const r = Math.round(s - m * 60);
  return r >= 10 ? `about ${m} min ${r} s` : `about ${m} minutes`;
}

function Progress({ r }: { r: TraceResult }) {
  const frac = r.planned > 0 ? Math.min(1, r.rounds / r.planned) : 0;
  const hops = r.hops?.length ?? 0;
  return (
    <div className="trace-progress" role="status" aria-live="polite">
      <div className="bar">
        <div style={{ width: `${frac * 100}%` }} />
      </div>
      <span className="muted">
        {r.rounds === 0 ? (
          <>
            <Loader2 size={13} className="spin" /> Finding the route to {r.host || r.target}…
          </>
        ) : (
          <>
            Round <b className="num">{r.rounds}</b> of {r.planned} · {hops} {hops === 1 ? "hop" : "hops"} so far
          </>
        )}
      </span>
    </div>
  );
}

function tone(r: TraceResult): "good" | "warn" | "bad" | "none" {
  if (!r.reached) return "none";
  if (r.loss_from == null) return (r.dest_loss_pct ?? 0) > 0 ? "warn" : "good";
  return "bad";
}

function Verdict({ r }: { r: TraceResult }) {
  return (
    <div className={`trace-verdict t-${tone(r)}`}>
      <div className="t">{r.verdict || "No verdict this time."}</div>
      {r.notes && r.notes.length > 0 && (
        <ul>
          {r.notes.map((n, i) => (
            <li key={i}>{n}</li>
          ))}
        </ul>
      )}
    </div>
  );
}

function HopTable({ r }: { r: TraceResult }) {
  const rows = [...(r.hops ?? []), ...(r.end ? [r.end] : [])];
  const noDest = !r.running && !r.reached;
  return (
    <div className="trace-table-wrap">
      <table className="trace-table">
        <thead>
          <tr>
            <th className="n">#</th>
            <th>Host</th>
            <th className="v" title="Probes this hop didn't answer, as mtr and WinMTR show it">
              Loss
            </th>
            <th className="v" title="Loss that carries on from this hop to the destination: the loss your traffic gets">
              Real loss
            </th>
            <th className="v">Avg</th>
            <th className="v">Best</th>
            <th className="v">Worst</th>
          </tr>
        </thead>
        <tbody>
          {rows.map((h) => (
            <HopRow key={`${h.probe}-${h.ttl}`} h={h} r={r} noDest={noDest} />
          ))}
        </tbody>
      </table>
    </div>
  );
}

function HopRow({ h, r, noDest }: { h: TraceHop; r: TraceResult; noDest: boolean }) {
  const udp = h.probe === "udp";
  const silent = h.recv === 0;
  const lossBad = h.persist_pct != null && h.persist_pct >= r.threshold_pct;
  const host = udp ? (r.end_name ?? "Relay UDP port") : h.name || h.addr;
  const sub = udp ? "" : h.name ? h.addr : h.private ? "private address" : "";
  const persistWhy = noDest ? "The destination didn't answer, so no loss can be confirmed" : r.running ? "Known when the trace finishes" : undefined;
  return (
    <tr className={`${h.loss_starts ? "starts" : ""}${silent ? " silent" : ""}`}>
      <td className="n num">{udp ? "UDP" : h.ttl}</td>
      <td className="host">
        {silent && !udp ? (
          <span className="faint" title={h.error || "This router doesn't answer trace probes. Many don't; it still forwards traffic."}>
            No reply
          </span>
        ) : (
          <>
            <span className="addr" title={h.others?.length ? `Also answered from: ${h.others.join(", ")}` : undefined}>
              {host}
              {h.others && h.others.length > 0 && <span className="faint"> +{h.others.length}</span>}
            </span>
            {sub && <span className="sub faint">{sub}</span>}
          </>
        )}
        {h.loss_starts && <span className="tag tag-bad">loss starts</span>}
        {h.limited && (
          <span className="tag" title="This router answers fewer trace probes than get through it. It limits replies; your traffic isn't lost here.">
            limits replies
          </span>
        )}
      </td>
      <td className={`v num${h.limited ? " faint" : h.loss_pct > 0 && !silent ? " q-warn" : ""}`}>
        {h.sent > 0 ? `${pct(h.loss_pct)}%` : "—"}
      </td>
      <td className={`v num${lossBad ? " q-bad" : ""}`} title={h.persist_pct == null ? persistWhy : undefined}>
        {h.persist_pct == null ? "—" : `${pct(h.persist_pct)}%`}
      </td>
      <td className="v num">{fmtMs(h.avg_ms)}</td>
      <td className="v num faint">{fmtMs(h.best_ms)}</td>
      <td className="v num faint">{fmtMs(h.worst_ms)}</td>
    </tr>
  );
}

function Footer({ r }: { r: TraceResult }) {
  const when = new Date(r.finished_at);
  const how = r.mode === "udp" ? "ICMP hops, UDP to the relay" : "ICMP";
  return (
    <div className="trace-foot">
      <span className="faint">
        {r.host && r.host !== r.target ? `${r.host} (${r.target})` : r.target} · {how} · {r.rounds} rounds
        {isNaN(when.getTime()) ? "" : ` · ${when.toLocaleString()}`}
      </span>
      <a className="btn btn-sm" href="/api/diag/trace/report" download="lowpath-route-trace.txt">
        <Download size={14} /> Export report
      </a>
    </div>
  );
}

