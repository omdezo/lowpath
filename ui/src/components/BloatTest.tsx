import { useCallback, useEffect, useRef, useState } from "react";
import { Gauge, Loader2, Play, Square } from "lucide-react";
import { api, describeError } from "../api";
import type { BloatPhase, BloatResult, BloatStatus, Grade, LatencyStats } from "../types";
import { fmtMbps, fmtMs } from "../util";
import "../diag.css";

// The active bufferbloat test: 20 s of idle / download / upload / recovery
// with latency probes throughout, graded A+ to F by how much the ping rises
// while the line is full. Unknown numbers show "—", never a guess.

const PHASES: { id: Exclude<BloatPhase, "picking">; label: string }[] = [
  { id: "idle", label: "Idle" },
  { id: "download", label: "Download" },
  { id: "upload", label: "Upload" },
  { id: "recovery", label: "Recovery" },
];

const POLL_MS = 400;

const plus = (v: number | null | undefined) => (v == null ? "—" : `+${fmtMs(v)}`);

function gradeTone(g: Grade): "good" | "warn" | "bad" | "none" {
  if (g === "A+" || g === "A") return "good";
  if (g === "B") return "warn";
  if (g === "") return "none";
  return "bad";
}

function verdict(r: BloatResult): string {
  switch (r.grade) {
    case "A+":
    case "A":
      return "Your ping stays steady even when the connection is full. Downloads won't make games lag.";
    case "B":
      return "Your ping rises a little when the connection is full. Big downloads during a match can cause some lag.";
    case "C":
    case "D":
    case "F":
      return "Your ping jumps when something fills the connection. Pause downloads and streams while you play, or turn on Smart Queue / SQM (fq_codel or CAKE) on your router.";
    default:
      return "No grade this time.";
  }
}

interface Props {
  onError: (msg: string, title?: string) => void;
}

export function BloatTest({ onError }: Props) {
  const [st, setSt] = useState<BloatStatus | null>(null);
  const [busy, setBusy] = useState(false);
  const timer = useRef<number | undefined>(undefined);
  const alive = useRef(true);

  const poll = useCallback(async () => {
    window.clearTimeout(timer.current);
    try {
      const s = await api.bloat();
      if (!alive.current) return;
      setSt(s);
      if (s.running) timer.current = window.setTimeout(poll, POLL_MS);
    } catch {
      // The live stream already shows "lost connection"; try again later.
      if (alive.current) timer.current = window.setTimeout(poll, 2000);
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

  const start = async () => {
    setBusy(true);
    try {
      setSt(await api.startBloat());
      timer.current = window.setTimeout(poll, POLL_MS);
    } catch (e) {
      const d = describeError(e, "Couldn't start the bufferbloat test");
      onError(d.msg, d.title);
    } finally {
      setBusy(false);
    }
  };

  const stop = async () => {
    setBusy(true);
    try {
      setSt(await api.stopBloat());
    } catch (e) {
      onError(describeError(e, "Couldn't stop the test").msg);
    } finally {
      setBusy(false);
    }
  };

  const running = !!st?.running;
  const r = st?.result ?? null;

  return (
    <section className="bloat" aria-label="Bufferbloat test">
      <div className="bloat-head">
        <span className="bloat-ico">
          <Gauge size={18} />
        </span>
        <div className="txt">
          <div className="t">Bufferbloat test</div>
          <div className="d">
            Fills your connection for 20 seconds, first downloading and then uploading, and measures how much your ping rises. That rise is
            the lag a game gets when a download or stream runs on your network. Uses roughly 50 MB to 1 GB, depending on your line speed.
          </div>
        </div>
        {running ? (
          <button className="btn" onClick={stop} disabled={busy}>
            <Square size={14} /> Stop
          </button>
        ) : (
          <button className="btn btn-primary" onClick={start} disabled={busy || !st}>
            {busy ? <Loader2 size={14} className="spin" /> : <Play size={14} />} {r ? "Run again" : "Run test"}
          </button>
        )}
      </div>

      {running && st?.progress && <Running p={st.progress} />}
      {!running && st?.error && <div className="note-warn bloat-note">{st.error}</div>}
      {!running && r && <Result r={r} />}
      {!running && !r && st && !st.error && <div className="bloat-empty muted">No test yet. Close other downloads first for a clean result.</div>}
    </section>
  );
}

function Running({ p }: { p: NonNullable<BloatStatus["progress"]> }) {
  const idx = PHASES.findIndex((x) => x.id === p.phase);
  const per = p.total_s / PHASES.length || 5;
  return (
    <div className="bloat-run" role="status" aria-live="polite">
      <div className="bloat-phases">
        {PHASES.map((ph, i) => {
          const fill = i < idx ? 1 : i === idx ? Math.min(1, Math.max(0, (p.elapsed_s - i * per) / per)) : 0;
          return (
            <div key={ph.id} className={`bloat-phase${i === idx ? " on" : ""}`}>
              <div className="bar">
                <div style={{ width: `${fill * 100}%` }} />
              </div>
              <span>{ph.label}</span>
            </div>
          );
        })}
      </div>
      <div className="bloat-live">
        {p.phase === "picking" ? (
          <span className="muted">
            <Loader2 size={13} className="spin" /> Finding something to ping…
          </span>
        ) : (
          <>
            <span>
              Ping now <b className="num">{fmtMs(p.last_ms)}</b> ms
            </span>
            {(p.phase === "download" || p.phase === "upload") && (
              <span>
                {p.phase === "download" ? "Downloading" : "Uploading"} <b className="num">{fmtMbps(p.load_mbps)}</b> Mbit/s
              </span>
            )}
            {p.target && <span className="faint bloat-target">to {p.target}</span>}
          </>
        )}
      </div>
    </div>
  );
}

function Result({ r }: { r: BloatResult }) {
  const tone = gradeTone(r.grade);
  const gw = (phase: string) => r.phases?.find((p) => p.phase === phase)?.gateway ?? null;
  const gwIdle = gw("idle");
  const gwRise = (phase: string) => rise(gw(phase), gwIdle);
  const when = new Date(r.finished_at);
  return (
    <div className="bloat-result">
      <div className="bloat-verdict">
        <div className={`bloat-grade g-${tone}`} aria-label={r.grade ? `Grade ${r.grade}` : "No grade"}>
          {r.grade || "—"}
        </div>
        <div>
          <div className="t">{verdict(r)}</div>
          {r.note && <div className="d">{r.note}</div>}
        </div>
      </div>

      <div className="bloat-stats">
        <Stat k="Idle ping" v={fmtMs(r.idle_ms)} unit="ms" s="before loading the line" />
        <Stat k="Download" v={plus(r.down_increase_ms)} unit="ms" s={`at ${fmtMbps(r.down_mbps)} Mbit/s`} />
        <Stat k="Upload" v={plus(r.up_increase_ms)} unit="ms" s={`at ${fmtMbps(r.up_mbps)} Mbit/s`} />
      </div>

      <dl className="bloat-facts">
        <dt>Measured to</dt>
        <dd>{r.target || "—"}</dd>
        <dt>Home network</dt>
        <dd>
          {gwIdle ? (
            <>
              Router {fmtMs(gwIdle.p50_ms)} ms idle, {plus(gwRise("download"))} ms downloading, {plus(gwRise("upload"))} ms uploading
              <span className="faint"> · {r.gateway}</span>
            </>
          ) : (
            "— (the router didn't answer ping)"
          )}
        </dd>
        <dt>Connection</dt>
        <dd>{connection(r)}</dd>
        <dt>Data used</dt>
        <dd>{Math.round(r.data_mb)} MB</dd>
        <dt>Tested</dt>
        <dd>{isNaN(when.getTime()) ? "—" : when.toLocaleString()}</dd>
      </dl>
    </div>
  );
}

function rise(p: LatencyStats | null, idle: LatencyStats | null): number | null {
  if (!p || !idle || p.p50_ms == null || idle.p50_ms == null) return null;
  return Math.max(0, p.p50_ms - idle.p50_ms);
}

// "Wi-Fi · WLAN 2", or just "Ethernet" when the adapter has that name.
function connection(r: BloatResult) {
  const kind = r.link === "wifi" ? "Wi-Fi" : r.link === "ethernet" ? "Ethernet" : "";
  const parts = [kind, r.interface].filter(Boolean);
  if (parts.length === 2 && parts[0].toLowerCase() === parts[1].toLowerCase()) parts.pop();
  return parts.join(" · ") || "—";
}

function Stat({ k, v, unit, s }: { k: string; v: string; unit: string; s: string }) {
  return (
    <div className="stat">
      <div className="k">{k}</div>
      <div className="v num">
        {v}
        {v !== "—" && <small>{unit}</small>}
      </div>
      <div className="s">{s}</div>
    </div>
  );
}
