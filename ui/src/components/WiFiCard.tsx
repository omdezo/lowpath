import { useCallback, useEffect, useRef, useState } from "react";
import { Loader2, Radar, Wifi } from "lucide-react";
import { api } from "../api";
import type { WiFiLink, WiFiSurveyView, WiFiTuning, WiFiView } from "../types";
import "../diag.css";

// The Wi-Fi card: signal, band and link rate of the Wi-Fi adapter, read
// from the system every few seconds, and on request the access point's own
// load report. Nothing here is estimated: a value the system didn't report
// is "—" with the reason.

const POLL_MS = 3000;

type Tone = "good" | "warn" | "bad" | "none";

// Signal words follow the usual RSSI bands: above -60 dBm strong, down to
// -70 good, below -70 weak (where the verdict starts blaming the radio),
// below -80 very weak.
function signalWords(rssi: number | null): { tone: Tone; word: string } {
  if (rssi == null) return { tone: "none", word: "Signal strength unknown" };
  if (rssi >= -60) return { tone: "good", word: "Strong signal" };
  if (rssi >= -70) return { tone: "good", word: "Good signal" };
  if (rssi >= -80) return { tone: "warn", word: "Weak signal" };
  return { tone: "bad", word: "Very weak signal" };
}

const num = (v: number | null | undefined, digits = 0) => (v == null ? "—" : v.toLocaleString(undefined, { maximumFractionDigits: digits }));

// "5,180 MHz", or every link of a Wi-Fi 7 multi-link connection.
function frequency(l: WiFiLink): string {
  if (l.mlo && l.links && l.links.length > 1) {
    return l.links.map((k) => `${num(k.freq_mhz)} MHz${k.rssi_dbm != null ? ` (${k.rssi_dbm} dBm)` : ""}`).join(" + ");
  }
  return l.freq_mhz != null ? `${num(l.freq_mhz)} MHz` : "—";
}

export function WiFiCard({ tuningEnabled }: { tuningEnabled: boolean }) {
  const [v, setV] = useState<WiFiView | null>(null);
  const [failed, setFailed] = useState(false);
  const [surveying, setSurveying] = useState(false);
  const timer = useRef<number | undefined>(undefined);
  const alive = useRef(true);

  const poll = useCallback(async () => {
    window.clearTimeout(timer.current);
    try {
      const r = await api.wifi();
      if (!alive.current) return;
      setV(r);
      setFailed(false);
    } catch {
      if (alive.current) setFailed(true);
    }
    if (alive.current) timer.current = window.setTimeout(poll, POLL_MS);
  }, []);

  useEffect(() => {
    alive.current = true;
    poll();
    return () => {
      alive.current = false;
      window.clearTimeout(timer.current);
    };
  }, [poll]);

  const survey = async () => {
    setSurveying(true);
    try {
      const r = await api.wifiSurvey();
      if (alive.current) setV(r);
    } catch {
      if (alive.current) setFailed(true);
    } finally {
      if (alive.current) setSurveying(false);
    }
  };

  const l = v?.state === "ok" ? v.link : null;
  const sig = signalWords(l?.rssi_dbm ?? null);

  return (
    <section className="bloat wifi" aria-label="Wi-Fi link" data-testid="wifi-card">
      <div className="bloat-head">
        <span className="bloat-ico">
          <Wifi size={18} />
        </span>
        <div className="txt">
          <div className="t">Wi-Fi link</div>
          <div className="d">Signal, band and speed of the link to your router, read from the system every few seconds.</div>
        </div>
        <button className="btn" onClick={survey} disabled={!l || surveying} title="Read the access point's load and the networks around it from the adapter's last scan">
          {surveying ? <Loader2 size={15} className="spin" /> : <Radar size={15} />} Check channel
        </button>
      </div>

      {!v && !failed && <div className="bloat-empty muted">Reading the Wi-Fi link…</div>}
      {failed && <div className="note-warn bloat-note">— The Lowpath service didn't answer; trying again.</div>}

      {v && !l && (
        <div className="bloat-result">
          <div className="bloat-verdict">
            <div className="bloat-grade g-none" aria-label="No reading">
              —
            </div>
            <div>
              <div className="t">{v.route === "ethernet" ? "Not on Wi-Fi" : "No Wi-Fi reading"}</div>
              <div className="d">{v.message || "The system didn't report the Wi-Fi link."}</div>
            </div>
          </div>
        </div>
      )}

      {l && v && (
        <div className="bloat-result">
          <div className="bloat-verdict">
            <div className={`bloat-grade g-${sig.tone}`} aria-label={l.rssi_dbm != null ? `${l.rssi_dbm} dBm` : "Signal unknown"}>
              {l.rssi_dbm ?? "—"}
            </div>
            <div>
              <div className="t">
                {sig.word}
                {l.band ? ` on ${l.band} GHz` : ""}
              </div>
              <div className="d">
                {v.message ||
                  (l.band === "2.4"
                    ? "2.4 GHz reaches far but is crowded and slow; a 5 GHz network on the same router usually lags less."
                    : "dBm: closer to 0 is stronger. Below −70 dBm games start to lose packets.")}
              </div>
            </div>
          </div>

          <div className="bloat-stats">
            <Stat k="Signal" v={num(l.rssi_dbm)} unit="dBm" s={l.quality_pct != null ? `quality ${l.quality_pct}%` : "—"} />
            <Stat k="Channel" v={l.channel != null ? String(l.channel) : "—"} unit="" s={[l.band ? `${l.band} GHz` : "", l.width_mhz ? `${l.width_mhz} MHz wide` : ""].filter(Boolean).join(" · ") || "—"} />
            <Stat k="Link rate" v={num(l.tx_mbps)} unit="Mbit/s" s={l.rx_mbps != null ? `send · ${num(l.rx_mbps)} receive` : "send"} />
          </div>

          <dl className="bloat-facts">
            <dt>Adapter</dt>
            <dd>{[l.interface, l.adapter].filter(Boolean).join(" · ") || "—"}</dd>
            <dt>Standard</dt>
            <dd>
              {[l.standard, l.generation].filter(Boolean).join(" · ") || "—"}
              {l.mlo && ` · multi-link (${l.links?.length ?? 0} links)`}
            </dd>
            <dt>Frequency</dt>
            <dd>{frequency(l)}</dd>
            <dt>Channel load</dt>
            <dd>
              <SurveyText s={v.survey} />
            </dd>
            <dt>Scan pause</dt>
            <dd>
              <TuningText t={v.tuning} enabled={tuningEnabled} />
            </dd>
          </dl>
          {l.notes && l.notes.length > 0 && (
            <ul className="wifi-notes faint">
              {l.notes.map((n, i) => (
                <li key={i}>{n}</li>
              ))}
            </ul>
          )}
        </div>
      )}
    </section>
  );
}

function SurveyText({ s }: { s: WiFiSurveyView | null }) {
  if (!s) return <>— (press Check channel: it reads the adapter's last scan, without scanning)</>;
  if (s.state !== "ok" || !s.result) return <>— ({s.message || "the scan couldn't be read"})</>;
  const r = s.result;
  const neighbors =
    r.same_channel === 0
      ? "no other network on this channel"
      : `${r.same_channel} other network${r.same_channel === 1 ? "" : "s"} on this channel` +
        (r.loudest_neighbor_dbm != null ? ` (loudest ${r.loudest_neighbor_dbm} dBm)` : "");
  const overlap = r.overlapping > 0 ? `, ${r.overlapping} overlapping` : "";
  return (
    <>
      {r.load ? (
        <>
          <b>{Math.round(r.load.busy_pct)}% busy</b> by the access point's own count, {r.load.stations} device{r.load.stations === 1 ? "" : "s"} on it ·{" "}
        </>
      ) : (
        <>— load ({r.load_note || "not reported"}) · </>
      )}
      {neighbors}
      {overlap}
      <span className="faint"> · {r.networks} networks heard</span>
    </>
  );
}

function TuningText({ t, enabled }: { t: WiFiTuning | null; enabled: boolean }) {
  if (!t) return <>{enabled ? "Background scans pause while a game is boosted over Wi-Fi." : "Off (Settings → Optimization)."}</>;
  switch (t.state) {
    case "applying":
      return <>Pausing background scans…</>;
    case "on":
      return <b>Background scans paused and streaming mode on for this boost.</b>;
    case "partial":
      return (
        <>
          {t.scan_paused ? "Background scans paused" : "Streaming mode on"}; {t.notes?.join(" ") || "the other setting couldn't be applied."}
        </>
      );
    case "failed":
      return <>Not paused: {t.message || t.notes?.join(" ") || "the adapter refused."}</>;
    default:
      return <>Reverted.</>;
  }
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
