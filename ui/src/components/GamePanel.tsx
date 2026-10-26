import { useCallback, useEffect, useMemo, useState, type CSSProperties, type ReactNode } from "react";
import { ChevronDown, Info, Loader2, Plus, Power, RotateCw, Wifi, Zap } from "lucide-react";
import { api, describeError } from "../api";
import type { Analysis, Game, Live, RelayView, Route, RouteMode, Settings, Stats } from "../types";
import { ms, pct, quality, score } from "../util";
import { StatusPill } from "./Bits";
import { ArtImg, GameIcon, hasArt } from "./GameArt";
import { LiveGraph, type Point } from "./LiveGraph";
import { RouteMap } from "./RouteMap";
import { TrafficCard } from "./Traffic";
import { ValveServers } from "./ValveServers";

interface Props {
  game: Game;
  live: Live;
  settings: Settings;
  relays: RelayView[];
  demo: boolean;
  history: Point[];
  now: number;
  otherGameName: string | null;
  onError: (msg: string, title?: string) => void;
  onOpenRelays: () => void;
  onEnableLoopback: () => Promise<void>;
}

const DUP_LABEL: Record<number, string> = { 0: "Auto", 1: "Off", 2: "Balanced", 4: "Max" };

// The server picker's value: "smart", "region:<continent>" or "route:<id>".
type Choice = string;

function parseChoice(c: Choice): { mode: RouteMode; region?: string; routeId?: string } {
  if (c.startsWith("region:")) return { mode: "region", region: c.slice(7) };
  if (c.startsWith("route:")) return { mode: "specific", routeId: c.slice(6) };
  return { mode: "smart" };
}

function choiceOf(mode: RouteMode, region?: string, routeId?: string): Choice {
  if (mode === "region") return `region:${region ?? ""}`;
  if (mode === "specific") return `route:${routeId ?? ""}`;
  return "smart";
}

// Jitter in words: what a player actually notices.
function stability(s?: Stats): { word: string; q: "good" | "warn" | "bad" } | null {
  if (!s?.up) return null;
  if (s.jitter_ms < 2 && s.loss_pct < 0.5) return { word: "Rock solid", q: "good" };
  if (s.jitter_ms < 5 && s.loss_pct < 1) return { word: "Stable", q: "good" };
  if (s.jitter_ms < 12 && s.loss_pct < 3) return { word: "A bit shaky", q: "warn" };
  return { word: "Unstable", q: "bad" };
}

const DETAILS_KEY = "lowpath.details";

function readDetailsOpen(): boolean {
  try {
    return localStorage.getItem(DETAILS_KEY) === "1";
  } catch {
    return false;
  }
}

export function GamePanel(props: Props) {
  const { game, live, settings, relays, demo, history, now, otherGameName, onError, onOpenRelays, onEnableLoopback } = props;
  const [routes, setRoutes] = useState<Route[]>([]);
  const [analysis, setAnalysis] = useState<Analysis | null>(null);
  const [analyzing, setAnalyzing] = useState(false);
  const [busy, setBusy] = useState<"apply" | "stop" | null>(null);
  const [choice, setChoice] = useState<Choice>("smart");
  const [details, setDetails] = useState(readDetailsOpen);

  const sess = live.session?.game_id === game.id ? live.session : null;
  const snap = sess ? live.snapshot : null;

  // Real relays are needed outside the demo; without them BOOST can only fail.
  const needsRelays = !demo && relays.length === 0 && !sess;
  const loopbackOnly = !demo && relays.length > 0 && relays.every((r) => r.builtin);
  const relayKey = relays.map((r) => r.id).join(",");
  const captureMode = settings.capture_mode ?? "proxy";
  const routeCapture = !!snap && (snap.mode === "capture" || sess?.capture_mode === "route");
  const servers = snap?.servers ?? [];
  const waiting = routeCapture && servers.length === 0;
  const processes = game.processes ?? [];
  const picked = parseChoice(choice);
  // Valve games: their relay POPs are known before the game starts.
  const sdr = !!game.discovery?.includes("sdr");

  const analyze = useCallback(async () => {
    setAnalyzing(true);
    try {
      setAnalysis(await api.analyze(game.id));
    } catch (e) {
      const d = describeError(e, "Couldn't measure the routes");
      onError(d.msg, d.title);
    } finally {
      setAnalyzing(false);
    }
  }, [game.id, onError]);

  // Load the game's routes (again when the relay list changes) and measure
  // them right away, so the numbers are there before the player asks.
  useEffect(() => {
    let cancelled = false;
    setRoutes([]);
    setAnalysis(null);
    api
      .routes(game.id)
      .then((r) => {
        if (cancelled) return;
        const list = r.routes ?? [];
        setRoutes(list);
        if (live.session?.game_id !== game.id) setChoice(game.smart ? "smart" : `region:${list[0]?.continent ?? ""}`);
        const fresh = r.analysis && Date.now() - new Date(r.analysis.at).getTime() < 60_000;
        if (fresh) setAnalysis(r.analysis!);
        else if (settings.auto_analyze !== false && game.routable && list.length > 0 && live.session?.game_id !== game.id) analyze();
      })
      .catch(() => !cancelled && setRoutes([]));
    return () => {
      cancelled = true;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [game.id, relayKey]);

  // Coming back to the optimized game shows the route it is really using.
  useEffect(() => {
    if (sess) setChoice(choiceOf(sess.mode, sess.region, sess.route_id));
  }, [sess?.started_at]); // eslint-disable-line react-hooks/exhaustive-deps

  const continents = useMemo(() => [...new Set(routes.map((r) => r.continent).filter(Boolean))], [routes]);
  // Region picks are offered when they mean something (several continents),
  // or when they are what the game or the running session uses.
  const regionOpts = continents.length > 1 ? continents : picked.mode === "region" ? [picked.region ?? ""] : [];

  // Per-route numbers: live while optimizing this game, else last analysis.
  const statsFor = useCallback(
    (id: string): (Stats & { active?: boolean }) | undefined => {
      if (snap) return (snap.paths ?? []).find((p) => p.id === id);
      return analysis?.paths?.[id];
    },
    [snap, analysis],
  );

  // Routes the current pick may use, best first by the last measurement.
  const eligible = useMemo(() => {
    let list = routes;
    if (picked.mode === "region") list = routes.filter((r) => r.continent === picked.region);
    if (picked.mode === "specific") list = routes.filter((r) => r.id === picked.routeId);
    if (!analysis) return list;
    return [...list].sort((a, b) => score(analysis.paths?.[a.id]) - score(analysis.paths?.[b.id]));
  }, [routes, picked.mode, picked.region, picked.routeId, analysis]);

  const best = eligible.find((r) => statsFor(r.id)?.up) ?? eligible[0];
  const bestStats = best ? statsFor(best.id) : undefined;

  const apply = async (c: Choice = choice) => {
    const p = parseChoice(c);
    setBusy("apply");
    try {
      await api.apply(game.id, p.mode, p.region, p.routeId);
    } catch (e) {
      const d = describeError(e, "Couldn't start optimizing");
      onError(d.msg, d.title);
    } finally {
      setBusy(null);
    }
  };

  const stop = async () => {
    setBusy("stop");
    try {
      await api.stop();
    } catch (e) {
      onError(describeError(e, "Couldn't stop optimizing").msg);
    } finally {
      setBusy(null);
    }
  };

  // Changing the server while boosting switches over at once: no second
  // button to find.
  const pick = (c: Choice) => {
    setChoice(c);
    if (sess && c !== choiceOf(sess.mode, sess.region, sess.route_id)) apply(c);
  };

  const toggleDetails = () => {
    setDetails((d) => {
      try {
        localStorage.setItem(DETAILS_KEY, d ? "0" : "1");
      } catch {
        // Only a convenience; the section still toggles.
      }
      return !d;
    });
  };

  // ---- numbers ---------------------------------------------------------

  const before = snap ? snap.before : analysis?.direct;
  const after = snap ? snap.after : bestStats;
  // "Before" is only a number when the game server answered our pings; a
  // missing reply must not turn into an invented baseline.
  const beforeKnown = snap ? (snap.before_known ?? snap.before?.up) : analysis ? (analysis.direct_known ?? analysis.direct?.up) : false;
  const beforeShown = before?.up && beforeKnown ? before : undefined;
  // Without the relay → server leg, "after" stops at the relay and isn't
  // comparable with a ping to the server.
  const afterToRelay = !demo && !!after?.up && (snap ? !snap.server_leg_known : !(after.server_ms && after.server_ms > 0));
  const improvement =
    beforeShown && after?.up && !afterToRelay && beforeShown.rtt_ms > 0 ? Math.round(((beforeShown.rtt_ms - after.rtt_ms) / beforeShown.rtt_ms) * 100) : null;
  // Sub-millisecond or few-percent gaps are probe noise; calling them faster
  // or slower would overstate what the route does.
  const sameAsDirect = improvement != null && Math.abs(beforeShown!.rtt_ms - after!.rtt_ms) < Math.max(1, beforeShown!.rtt_ms * 0.05);

  const leg = snap ? (snap.server_leg_known ? (snap.server_leg_ms ?? null) : null) : after?.server_ms && after.server_ms > 0 ? after.server_ms : null;
  const toRelay = after?.up ? Math.max(0, after.rtt_ms - (leg ?? 0)) : null;
  const activePaths = (snap?.paths ?? []).filter((p) => p.active);
  const relayLabel = activePaths.length > 1 ? `${activePaths.length} relays` : (activePaths[0]?.label ?? best?.label ?? "Relay");
  const primary = servers.find((s) => s.primary) ?? servers[0];
  const serverLabel = primary ? primary.addr.replace(/:\d+$/, "") : "Game server";
  const stab = stability(after);

  const pingCaption = (() => {
    if (snap?.fallback) return <span className="q-bad">Routes down: on your normal connection</span>;
    if (waiting) return "Waiting for the game";
    const normal = beforeShown ? `Normal route ${ms(beforeShown.rtt_ms)} ms` : "";
    if (sameAsDirect) return `${normal} · about the same`;
    if (improvement != null && improvement > 0)
      return (
        <>
          {normal} · <span className="q-good">{improvement}% lower</span>
        </>
      );
    if (improvement != null && improvement < 0)
      return (
        <>
          {normal} · <span className="q-warn">{-improvement}% higher</span>
        </>
      );
    if (!beforeShown && (snap || analysis)) {
      if (demo) return "Normal route not measured";
      if (snap) return "Server ignores ping";
      return after?.up ? `Relay ${ms(after.rtt_ms)} ms · game server not known yet` : "Game server not known yet";
    }
    if (afterToRelay) return "Measured to the relay";
    return analyzing ? "Measuring…" : "Not measured yet";
  })();

  // ---- the big button ----------------------------------------------------

  type BtnState = "setup" | "blocked" | "idle" | "starting" | "stopping" | "waiting" | "on" | "down";
  const btn: BtnState = needsRelays
    ? "setup"
    : busy === "stop"
      ? "stopping"
      : busy === "apply" || (sess && live.status === "applying")
        ? "starting"
        : snap?.fallback
          ? "down"
          : sess
            ? waiting
              ? "waiting"
              : "on"
            : !game.routable
              ? "blocked"
              : "idle";

  const onBig = () => {
    if (btn === "setup") onOpenRelays();
    else if (sess) stop();
    else apply();
  };

  const procText = processes.length ? processes.join(", ") : game.name;
  const caption: ReactNode = (() => {
    switch (btn) {
      case "setup":
        return "Lowpath needs a relay server to route your game through.";
      case "blocked":
        return captureMode === "route" ? (
          <>Lowpath doesn't know which program {game.name} runs as. Start it, then use <b>Add game</b>.</>
        ) : (
          <>Proxy mode needs {game.name}'s server address. Switch to Route mode in Settings → Network.</>
        );
      case "starting":
        return "Connecting to the relay…";
      case "stopping":
        return "Putting your traffic back on the normal route…";
      case "waiting":
        return (
          <>
            Now launch <b>{game.name}</b>. Lowpath picks up its traffic automatically.
          </>
        );
      case "down":
        return settings.kill_switch ? "Every route is down. Kill switch is holding game traffic until one recovers." : "Every route is down. Your game is on its normal route until one recovers.";
      case "on":
        if (demo) return <>Simulated game traffic is riding {relayLabel}.</>;
        if (routeCapture) return <>Routing <b>{procText}</b> through {relayLabel}.</>;
        return (
          <>
            Point {game.name} at <b className="num">{snap?.listen}</b>
          </>
        );
      default:
        if (analyzing) return "Measuring the routes…";
        if (otherGameName) return <>Boosting will move from {otherGameName} to {game.name}.</>;
        return demo || captureMode !== "route" ? "Press Boost to start." : <>Press Boost, then launch {game.name}.</>;
    }
  })();

  const isOn = btn === "on" || btn === "waiting" || btn === "down";
  // Before a session the relay-only number would pass for a game ping it
  // isn't; the caption gives it its real name instead.
  const showPing = !!after?.up && !(afterToRelay && !snap);
  const pill = snap?.fallback ? "error" : busy === "apply" || live.status === "applying" ? "applying" : analyzing && !sess ? "analyzing" : sess ? "optimizing" : "idle";
  const lossQ = after?.up ? (after.loss_pct >= 3 ? "bad" : after.loss_pct >= 1 ? "warn" : "good") : "";

  return (
    <div className="game">
      <Banner game={game} status={<StatusPill status={pill} />} />

      {btn === "setup" ? (
        <>
          <section className="card setup-wrap">
            <Setup gameName={game.name} onOpenRelays={onOpenRelays} onEnableLoopback={onEnableLoopback} />
          </section>
          {sdr && <ValveServers gameId={game.id} gameName={game.name} now={now} />}
        </>
      ) : (
        <>
          <section className={`card control ${isOn ? "control-on" : ""}`}>
            <div className="control-stats">
              <div className="stat-main">
                <div className="k">{isOn ? "Ping now" : "Expected ping"}</div>
                <div className="v num">
                  <span className={showPing ? `q-${quality(after)}` : "faint"}>{showPing ? ms(after!.rtt_ms) : "—"}</span>
                  {showPing && <small>ms</small>}
                </div>
                <div className="s">{pingCaption}</div>
              </div>
              <div className="stat-side">
                <div>
                  <div className="k">Packet loss</div>
                  <div className={`v num ${lossQ ? `q-${lossQ}` : "faint"}`}>
                    {after?.up ? pct(after.loss_pct) : "—"}
                    {after?.up && <small>%</small>}
                  </div>
                </div>
                <div>
                  <div className="k">Stability</div>
                  <div className={`v ${stab ? `q-${stab.q}` : "faint"}`}>{stab ? stab.word : "—"}</div>
                </div>
              </div>
            </div>
            <div className="control-actions">
              <label className="field-label" htmlFor="server-pick">
                Server
              </label>
              <select id="server-pick" className="select" value={choice} onChange={(e) => pick(e.target.value)} disabled={busy != null || routes.length === 0}>
                {game.smart && <option value="smart">Auto · best route{routes.length > 1 ? `s (${routes.length})` : ""}</option>}
                {regionOpts.map((c) => (
                  <option key={c} value={`region:${c}`}>
                    {continents.length > 1 ? `Best in ${c}` : "Best route"}
                  </option>
                ))}
                {routes.map((r) => {
                  const st = analysis?.paths?.[r.id];
                  return (
                    <option key={r.id} value={`route:${r.id}`}>
                      {r.label}
                      {st?.up ? ` · ${ms(st.rtt_ms)} ms` : ""}
                    </option>
                  );
                })}
              </select>
              <BoostButton state={btn} gameName={game.name} onClick={onBig} />
              <div className="control-caption" aria-live="polite">
                {isOn && btn !== "down" && <span className="live-dot" aria-hidden="true" />}
                <span>{caption}</span>
              </div>
            </div>
          </section>

          <RouteMap
            on={!!sess && !snap?.fallback}
            flowing={btn === "on"}
            relayLabel={relayLabel}
            serverLabel={serverLabel}
            toRelay={toRelay}
            relayToServer={leg}
            direct={beforeShown ? beforeShown.rtt_ms : null}
          />

          {sdr && <ValveServers gameId={game.id} gameName={game.name} now={now} />}

          {loopbackOnly && (
            <div className="note">
              <Wifi size={14} /> You're on the test relay on this PC: it proves the route works, but won't lower your ping.{" "}
              <button className="link" onClick={onOpenRelays}>
                Add a real server
              </button>
            </div>
          )}

          {snap && snap.after.up && snap.after.jitter_ms > 8 && (
            <div className="tip">
              <Info size={15} /> Jitter is high on every route. The cause may be local (Wi-Fi, or another device using bandwidth), which Lowpath can't fix.
            </div>
          )}

          <div className="more">
            <button className="more-toggle" onClick={toggleDetails} aria-expanded={details}>
              <ChevronDown size={16} className={details ? "rot" : ""} /> Details
              <span className="more-hint">graph, routes, traffic</span>
            </button>
            {details && (
              <div className="more-body">
                <div className="card graph-card">
                  <div style={{ display: "flex", alignItems: "center", marginBottom: 8 }}>
                    <span className="section-label">Live ping · last 60s</span>
                    <span className="graph-legend" style={{ marginLeft: "auto" }}>
                      <span>
                        <i style={{ background: "var(--graph-before)" }} />
                        Normal route
                      </span>
                      <span>
                        <i style={{ background: "var(--accent)" }} />
                        Lowpath{afterToRelay ? " (to relay)" : ""}
                      </span>
                    </span>
                  </div>
                  {snap ? (
                    <LiveGraph points={history} now={now} />
                  ) : (
                    <div className="empty-list" style={{ height: 150, display: "grid", placeItems: "center" }}>
                      The graph starts when you press Boost.
                    </div>
                  )}
                </div>

                {routeCapture && <TrafficCard gameName={game.name} processes={processes} servers={servers} capture={live.capture ?? null} now={now} />}

                <div className="routes">
                  <div className="routes-head section-label">
                    <span>Routes</span>
                    <span style={{ textAlign: "right" }}>Ping</span>
                    <span style={{ textAlign: "right" }}>Loss</span>
                    <span style={{ textAlign: "right" }}>Jitter</span>
                    <span />
                  </div>
                  {routes.map((r) => (
                    <RouteRow key={r.id} title={r.label} desc={[r.region, r.continent].filter(Boolean).join(" · ")} stats={statsFor(r.id)} loading={analyzing} active={statsFor(r.id)?.active} />
                  ))}
                  {routes.length === 0 && <div className="empty-list">No routes yet.</div>}
                </div>

                <div className="facts">
                  <span>
                    Duplication: <b>{DUP_LABEL[settings.dup_level]}</b>
                  </span>
                  <span>
                    Parallel routes: <b className="num">{snap ? `${snap.copies} of ${snap.paths?.length ?? 0}` : "—"}</b>
                  </span>
                  <span>
                    Duplicates dropped: <b className="num">{snap ? snap.dups_dropped.toLocaleString() : "—"}</b>
                  </span>
                  <span>
                    Capture: <b>{captureMode === "route" ? "Route mode" : "Proxy mode"}</b>
                  </span>
                  <span className="spacer" />
                  <button className="btn btn-sm" onClick={analyze} disabled={analyzing || !game.routable || !!snap}>
                    {analyzing ? <Loader2 size={14} className="spin" /> : <RotateCw size={14} />}
                    {analyzing ? "Measuring…" : "Re-test routes"}
                  </button>
                </div>
              </div>
            )}
          </div>
        </>
      )}
    </div>
  );
}

function BoostButton({ state, gameName, onClick }: { state: string; gameName: string; onClick: () => void }) {
  const on = state === "on" || state === "waiting" || state === "down";
  const busy = state === "starting" || state === "stopping";
  return (
    <button
      className={`boost-btn ${on ? "boost-btn-on" : ""}`}
      onClick={onClick}
      disabled={busy || state === "blocked"}
      aria-label={on ? `Stop optimizing ${gameName}` : `Boost ${gameName}`}
      aria-pressed={on}
    >
      {busy ? <Loader2 size={18} className="spin" /> : on ? <Power size={18} /> : <Zap size={18} />}
      {state === "starting" ? "Starting…" : state === "stopping" ? "Stopping…" : on ? "Stop" : "Boost"}
    </button>
  );
}

// Banner shows the game the way its launcher does: Steam's hero artwork with
// the transparent title logo on top, or the game's own icon and name.
function Banner({ game, status }: { game: Game; status: ReactNode }) {
  const [hero, setHero] = useState(() => hasArt(game.id, "hero"));
  const [logo, setLogo] = useState(() => hasArt(game.id, "logo"));
  return (
    <section className={`gbanner ${hero ? "" : "gbanner-plain"}`} style={{ "--game": game.color } as CSSProperties}>
      {hero && <ArtImg id={game.id} kind="hero" className="gbanner-img" onMissing={() => setHero(false)} />}
      <div className="gbanner-shade" />
      <div className="gbanner-status">{status}</div>
      <div className="gbanner-body">
        {logo ? (
          <>
            <h1 className="sr-only">{game.name}</h1>
            <ArtImg id={game.id} kind="logo" className="gbanner-logo" onMissing={() => setLogo(false)} />
          </>
        ) : (
          <h1 className="gbanner-title">
            <GameIcon game={game} size={44} />
            <span>{game.name}</span>
          </h1>
        )}
        <div className="gbanner-meta">
          {game.genre}
          {game.running && <span className="badge-live">● Running</span>}
        </div>
      </div>
    </section>
  );
}

// Shown in place of the big button until a relay exists, so the first thing a
// new player meets is a way forward rather than a button that can only fail.
function Setup({ gameName, onOpenRelays, onEnableLoopback }: { gameName: string; onOpenRelays: () => void; onEnableLoopback: () => Promise<void> }) {
  const [busy, setBusy] = useState(false);
  const enable = async () => {
    setBusy(true);
    try {
      await onEnableLoopback();
    } finally {
      setBusy(false);
    }
  };
  return (
    <div className="setup">
      <h2>One quick step before you boost</h2>
      <div className="muted">Lowpath sends {gameName}'s traffic through a relay server. Pick how you want to start.</div>
      <div className="setup-cards">
        <button className="setup-card" onClick={enable} disabled={busy}>
          <span className="setup-icon">{busy ? <Loader2 size={22} className="spin" /> : <Wifi size={22} />}</span>
          <span className="t">Try it on this PC</span>
          <span className="d">Turns on the built-in test relay. Proves everything works; it won't lower your ping.</span>
        </button>
        <button className="setup-card" onClick={onOpenRelays}>
          <span className="setup-icon">
            <Plus size={22} />
          </span>
          <span className="t">Add my server</span>
          <span className="d">Use a relay on your own VPS, close to the game's servers. This is what lowers ping.</span>
        </button>
      </div>
    </div>
  );
}

function RouteRow(props: { title: string; desc: string; stats?: Stats; loading?: boolean; active?: boolean }) {
  const { title, desc, stats, loading, active } = props;
  const q = stats ? quality(stats) : null;
  const metric = (v: string, unit: string) =>
    loading && !stats ? (
      <span className="metric">
        <span className="skeleton" />
      </span>
    ) : (
      <span className="metric num">
        {stats ? (stats.up ? v : "—") : "—"}
        {stats?.up && <small>{unit}</small>}
      </span>
    );
  return (
    <div className="route-card">
      <span className="label">
        <span style={{ width: 18 }} />
        <span style={{ minWidth: 0 }}>
          <span className="title" style={{ display: "block" }}>
            {title}
            {active && <span className="carrying">● live</span>}
          </span>
          <span className="desc">{desc}</span>
        </span>
      </span>
      {metric(stats ? ms(stats.rtt_ms) : "", "ms")}
      {metric(stats ? pct(stats.loss_pct) : "", "%")}
      {metric(stats ? ms(stats.jitter_ms) : "", "ms")}
      <span style={{ display: "grid", placeItems: "center" }} title={q ? `Quality: ${q}` : undefined}>
        {q && !loading ? <span className={`qdot q-${q}`} /> : <span className="qdot" style={{ color: "var(--border-strong)" }} />}
      </span>
    </div>
  );
}
