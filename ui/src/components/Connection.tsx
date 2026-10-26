import { useCallback, useEffect, useRef, useState } from "react";
import { AlertTriangle, Globe, Loader2, RefreshCw } from "lucide-react";
import { api } from "../api";
import type { Endpoint, NetLocation } from "../types";
import { cap } from "../util";

// Country name from an ISO code ("NL" → "Netherlands"); the code itself when the
// runtime has no display names.
function countryName(code?: string): string {
  if (!code) return "";
  try {
    return new Intl.DisplayNames(undefined, { type: "region" }).of(code.toUpperCase()) ?? code;
  } catch {
    return code;
  }
}

// "Amsterdam, Netherlands", "Netherlands", or "" when unknown.
export function place(e?: Endpoint | null): string {
  if (!e) return "";
  const country = countryName(e.country);
  return [e.city, country].filter(Boolean).join(", ");
}

// "Example Broadband (AS64500)", or "" when the AS is unknown.
export function network(e?: Endpoint | null): string {
  if (!e?.asn) return "";
  const name = e.isp || e.as_name || "";
  return name ? `${name} (AS${e.asn})` : `AS${e.asn}`;
}

// The longest wait for an automatic second lookup; a larger hint is ignored.
const MAX_RETRY_S = 30;

// Connection shows where the user is on the internet: the place, ISP and AS
// of their public address, looked up when the pane opens. It never guesses:
// anything unknown is shown as "—" with the reason.
//
// When the ISP is unknown only because the registry was slow (the server
// sets retry_after_s), the card asks once more by itself after that wait,
// without refresh so the server's cache decides. It does that at most once
// per lookup the user started, so a registry that stays down is not polled.
export function Connection() {
  const [loc, setLoc] = useState<NetLocation | null>(null);
  const [busy, setBusy] = useState(false);
  const [retrying, setRetrying] = useState(false);
  const [err, setErr] = useState("");
  const alive = useRef(true);
  const retryTimer = useRef<number | undefined>(undefined);

  const cancelRetry = useCallback(() => {
    window.clearTimeout(retryTimer.current);
    retryTimer.current = undefined;
    setRetrying(false);
  }, []);

  const load = useCallback(
    async (refresh: boolean, auto = false) => {
      cancelRetry();
      setBusy(true);
      setErr("");
      try {
        const l = await api.location(refresh);
        if (!alive.current) return;
        setLoc(l);
        const wait = l.retry_after_s ?? 0;
        if (!auto && wait > 0 && wait <= MAX_RETRY_S) {
          setRetrying(true);
          // Two loads can overlap (StrictMode mounts twice in dev): keep
          // one timer, so there is one retry and unmount can still clear it.
          window.clearTimeout(retryTimer.current);
          retryTimer.current = window.setTimeout(() => {
            retryTimer.current = undefined;
            if (alive.current) load(false, true);
          }, wait * 1000 + 500);
        }
      } catch (e) {
        if (alive.current) setErr(e instanceof Error && e.message ? e.message : "The lookup failed.");
      } finally {
        if (alive.current) setBusy(false);
      }
    },
    [cancelRetry],
  );

  useEffect(() => {
    alive.current = true;
    load(false);
    return () => {
      alive.current = false;
      window.clearTimeout(retryTimer.current);
    };
  }, [load]);

  // The address the game's IPv4 traffic uses; IPv6 when that is all there is.
  const main = loc?.ipv4 ?? loc?.ipv6 ?? null;
  const where = place(main);
  const isp = network(main);
  const headline = !loc ? (busy ? "Looking up your connection…" : "—") : where || isp ? [where, isp].filter(Boolean).join(" · ") : "—";
  const reason = err || (loc && loc.state !== "ok" ? loc.message : "") || (main?.partial ? cap(main.partial) + "." : "");
  const again = retrying || (busy && !!loc?.retry_after_s);

  return (
    <div className="setting connection" aria-busy={busy || retrying} data-testid="connection">
      <span className="muted">
        <Globe size={18} />
      </span>
      <div className="txt">
        <div className="t">Your connection</div>
        <div className="conn-headline">{headline}</div>
        {/* Always laid out ("—" until known) so the pane doesn't jump when the lookup lands. */}
        <dl className="conn-facts">
          <Fact label="Public IPv4" value={loc?.ipv4?.ip} />
          <Fact label="Public IPv6" value={loc?.ipv6?.ip} title={loc && !loc.ipv6 ? loc.ipv6_note : undefined} />
          <Fact label="Network" value={main?.prefix} />
        </dl>
        {reason && (
          <div className="note-warn">
            <AlertTriangle size={14} />
            <span>
              {reason}
              {again && (/[.!?…]$/.test(reason) ? " " : ". ") + "Trying again…"}
            </span>
          </div>
        )}
        <div className="d">
          Found with a STUN request to Google or Cloudflare, which sees you the way a game server does, and the public RIPEstat
          registry (RIPE NCC). Only your address is sent to them.
        </div>
      </div>
      <button className="btn" onClick={() => load(true)} disabled={busy} title="Look up your connection again">
        {busy ? <Loader2 size={15} className="spin" /> : <RefreshCw size={15} />} Refresh
      </button>
    </div>
  );
}

function Fact({ label, value, title }: { label: string; value?: string; title?: string }) {
  return (
    <div title={title}>
      <dt>{label}</dt>
      <dd>{value || "—"}</dd>
    </div>
  );
}
