import { useCallback, useEffect, useRef, useState } from "react";
import { Loader2, Radio, Search, Settings as Gear, WifiOff, Zap } from "lucide-react";
import { api, ApiError, describeError, subscribeLive } from "./api";
import type { Bootstrap, Game, Live, Platform, RelayView, Settings } from "./types";
import { Logo } from "./components/Bits";
import { Mapping } from "./components/Mapping";
import { Library } from "./components/Library";
import { GamePanel } from "./components/GamePanel";
import { Home } from "./components/Home";
import { NetworkLab } from "./components/NetworkLab";
import { SettingsModal, type SettingsTab } from "./components/SettingsModal";
import { AddGameModal } from "./components/AddGame";
import { CaptureBanner } from "./components/CaptureBanner";
import { LineBanner } from "./components/LineBanner";
import { FailsafeBanner } from "./components/FailsafeBanner";
import { Toasts, type Toast } from "./components/Toasts";
import type { Point } from "./components/LiveGraph";

type Phase = "loading" | "offline" | "unauthorized" | "mapping" | "main";

// The service answers 401 to a page that doesn't hold this run's session
// cookie: opened by hand, or left over from before Lowpath restarted.
const notConnected = (e: unknown) => e instanceof ApiError && e.status === 401;

export function App() {
  const [phase, setPhase] = useState<Phase>("loading");
  const [boot, setBoot] = useState<Bootstrap | null>(null);
  const [settings, setSettings] = useState<Settings | null>(null);
  const [games, setGames] = useState<Game[]>([]);
  const [recent, setRecent] = useState<string[]>([]);
  const [live, setLive] = useState<Live | null>(null);
  const [down, setDown] = useState(false);
  const [selected, setSelected] = useState<string | null>(null);
  const [query, setQuery] = useState("");
  const [relays, setRelays] = useState<RelayView[]>([]);
  const [platform, setPlatform] = useState<Platform | null>(null);
  const [showSettings, setShowSettings] = useState<SettingsTab | null>(null);
  const [showAddGame, setShowAddGame] = useState(false);
  const [toasts, setToasts] = useState<Toast[]>([]);
  // The fail-open notice the user closed (by its time): hidden here at once,
  // while the service forgets it for reloads and other windows.
  const [failsafeSeen, setFailsafeSeen] = useState("");
  const dismissFailsafe = useCallback((at: string) => {
    setFailsafeSeen(at);
    // If the service can't be reached, the notice stays hidden in this window.
    api.dismissFailsafe().then(setLive).catch(() => {});
  }, []);
  const [history, setHistory] = useState<Point[]>([]);
  const [now, setNow] = useState(Date.now());
  const searchRef = useRef<HTMLInputElement>(null);
  const seenEvents = useRef<{ session: string; count: number }>({ session: "", count: 0 });
  const toastId = useRef(0);
  const wasDown = useRef(false);

  // The route-mapping sweep needs something to probe; with no relays yet the
  // game panel's onboarding is the useful next step instead.
  const needsMapping = (b: Bootstrap) => !b.mapping && (b.demo || b.relays == null || b.relays.length > 0);

  const applyBoot = useCallback((b: Bootstrap) => {
    setBoot(b);
    setSettings(b.settings);
    setGames(b.games ?? []);
    setRelays(b.relays ?? []);
    setPlatform(b.platform ?? null);
  }, []);

  const load = useCallback(async () => {
    try {
      const b = await api.bootstrap();
      applyBoot(b);
      setRecent(b.recent ?? []);
      setLive(b.live);
      if (b.live?.session) setSelected(b.live.session.game_id);
      setPhase(needsMapping(b) ? "mapping" : "main");
    } catch (e) {
      setPhase(notConnected(e) ? "unauthorized" : "offline");
    }
  }, [applyBoot]);

  useEffect(() => {
    load();
  }, [load]);

  // After the service comes back (for example restarted as administrator),
  // platform, relays and settings may have changed under us.
  useEffect(() => {
    if (down) {
      wasDown.current = true;
      return;
    }
    if (!wasDown.current) return;
    wasDown.current = false;
    api.bootstrap().then(applyBoot, () => {});
  }, [down, applyBoot]);

  // The live stream can't tell "service gone" from "service restarted and
  // this page is no longer connected"; while it is down, ask the API.
  useEffect(() => {
    if (!down) return;
    const id = window.setInterval(() => {
      api.bootstrap().catch((e) => {
        if (notConnected(e)) setPhase("unauthorized");
      });
    }, 3000);
    return () => window.clearInterval(id);
  }, [down]);

  const refreshRelays = useCallback(async () => {
    try {
      const r = await api.relays();
      setRelays(r.relays ?? []);
    } catch {
      // Keep the last known list; the next bootstrap refresh corrects it.
    }
  }, []);

  useEffect(() => {
    document.documentElement.dataset.theme = settings?.theme ?? "dark";
  }, [settings?.theme]);

  const pushToast = useCallback((kind: Toast["kind"], msg: string, title?: string) => {
    const id = ++toastId.current;
    setToasts((t) => [...t.slice(-3), { id, kind, msg, title }]);
    // Errors usually say what to do next, so they stay up long enough to read.
    window.setTimeout(() => setToasts((t) => t.filter((x) => x.id !== id)), kind === "fallback" || kind === "error" ? 9000 : 4500);
  }, []);

  // Live stream: status, engine snapshot, running games.
  useEffect(() => {
    if (phase !== "main") return;
    return subscribeLive((l) => {
      const t = Date.now();
      setNow(t);
      setLive(l);
      const running = new Set(l.running ?? []);
      setGames((gs) => (gs.some((g) => g.running !== running.has(g.id)) ? gs.map((g) => ({ ...g, running: running.has(g.id) })) : gs));

      const key = l.session?.started_at ?? "";
      if (key !== seenEvents.current.session) {
        seenEvents.current = { session: key, count: 0 };
        setHistory([]);
      }
      if (l.snapshot) {
        const s = l.snapshot;
        const evs = s.events ?? [];
        evs.slice(seenEvents.current.count).forEach((e) => pushToast(e.kind, e.msg));
        seenEvents.current.count = evs.length;
        const beforeOk = s.before?.up && s.before_known !== false;
        setHistory((h) => [...h.filter((p) => p.t > t - 65_000), { t, before: beforeOk ? s.before.rtt_ms : null, after: s.after?.up ? s.after.rtt_ms : null }]);
      }
    }, setDown);
  }, [phase, pushToast]);

  // Ctrl/Cmd+K focuses the game search.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === "k") {
        e.preventDefault();
        searchRef.current?.focus();
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, []);

  // Keep time-based labels ticking even without live updates.
  useEffect(() => {
    const t = window.setInterval(() => setNow(Date.now()), 30_000);
    return () => window.clearInterval(t);
  }, []);

  const onFavorite = async (id: string) => {
    try {
      const favorites = (await api.favorite(id)).favorites ?? [];
      setGames((gs) => gs.map((g) => ({ ...g, favorite: favorites.includes(g.id) })));
    } catch (e) {
      pushToast("error", describeError(e, "Couldn't update favorites").msg);
    }
  };

  const selectGame = (id: string) => {
    setSelected(id);
    setRecent((r) => [id, ...r.filter((x) => x !== id)].slice(0, 8));
  };

  const onGameAdded = (g: Game) => {
    setShowAddGame(false);
    setGames((gs) => [...gs.filter((x) => x.id !== g.id), { ...g, custom: true }]);
    selectGame(g.id);
    pushToast("success", `${g.name} is in your library. Press BOOST, then launch it.`, "Game added");
    // The add response may not carry the library flags (running, routable…);
    // the bootstrap list is the source of truth.
    api.bootstrap().then((b) => setGames(b.games ?? []), () => {});
  };

  const removeGame = async (g: Game) => {
    if (!window.confirm(`Remove ${g.name} from your library?`)) return;
    try {
      await api.deleteCustomGame(g.id);
      setGames((gs) => gs.filter((x) => x.id !== g.id));
      setRecent((r) => r.filter((x) => x !== g.id));
      if (selected === g.id) setSelected(null);
    } catch (e) {
      const d = describeError(e, `Couldn't remove ${g.name}`);
      pushToast("error", d.msg, d.title);
    }
  };

  const enableLoopback = async () => {
    if (!settings) return;
    try {
      setSettings(await api.saveSettings({ ...settings, loopback_relay: true }));
      await refreshRelays();
    } catch (e) {
      const d = describeError(e, "Couldn't turn on the loopback test relay");
      pushToast("error", d.msg, d.title);
    }
  };

  const onError = useCallback((msg: string, title?: string) => pushToast("error", msg, title), [pushToast]);

  if (phase === "loading") {
    return (
      <div className="mapping">
        <Loader2 className="spin" color="var(--accent)" />
      </div>
    );
  }
  if (phase === "unauthorized") {
    return (
      <div className="mapping">
        <div className="mapping-inner">
          <WifiOff size={36} color="var(--bad)" />
          <h2 style={{ margin: "18px 0 4px", fontSize: 19 }}>This page isn't connected to Lowpath</h2>
          <div className="muted">
            Lowpath only answers its own window. Use the Lowpath window, or close Lowpath and start it again.
          </div>
        </div>
      </div>
    );
  }

  if (phase === "offline") {
    return (
      <div className="mapping">
        <div className="mapping-inner">
          <WifiOff size={36} color="var(--bad)" />
          <h2 style={{ margin: "18px 0 4px", fontSize: 19 }}>Can't reach the Lowpath service</h2>
          <div className="muted">Start Lowpath, then retry.</div>
          <button className="btn" style={{ marginTop: 18 }} onClick={load}>
            Retry
          </button>
        </div>
      </div>
    );
  }
  if (!boot || !settings) {
    return (
      <div className="mapping">
        <Loader2 className="spin" color="var(--accent)" />
      </div>
    );
  }
  if (phase === "mapping") {
    return <Mapping onDone={() => setPhase("main")} />;
  }

  const game = games.find((g) => g.id === selected) ?? null;
  const sessGame = live?.session ? games.find((g) => g.id === live.session!.game_id) : null;
  // Route mode is chosen but this process can't open the capture driver.
  const captureBlocked = !boot.demo && settings.capture_mode === "route" && !!platform && !platform.capture.available;

  return (
    <div className="app">
      <header className="topbar">
        <div className="brand">
          <Logo />
          Lowpath
        </div>
        <label className="search">
          <Search size={16} />
          <input ref={searchRef} placeholder="Search games…" value={query} onChange={(e) => setQuery(e.target.value)} aria-label="Search games" />
          {!query && <kbd>Ctrl K</kbd>}
        </label>
        <div className="topbar-right">
          {sessGame && (
            <button className="chip chip-accent" onClick={() => selectGame(sessGame.id)} title="Go to the optimized game">
              <Zap size={13} /> {sessGame.name}
            </button>
          )}
          {boot.demo && (
            <span className="chip" title="Relays, ISP route and game server are simulated on this PC">
              <Radio size={13} /> Simulated network
            </span>
          )}
          <button className="btn btn-ghost icon-btn" onClick={() => setShowSettings("network")} aria-label="Settings">
            <Gear size={18} />
          </button>
        </div>
      </header>

      <Library
        games={games}
        recent={recent}
        query={query}
        selected={selected}
        optimizingId={live?.session?.game_id ?? null}
        onSelect={selectGame}
        onFavorite={onFavorite}
        onAddGame={() => setShowAddGame(true)}
        onRemoveGame={removeGame}
      />

      <main className="main">
        {down && (
          <div className="banner banner-bad" style={{ margin: "16px 28px 0" }}>
            <WifiOff size={16} /> Lost connection to the Lowpath service. Reconnecting…
          </div>
        )}
        {captureBlocked && platform && (
          <div className="main-banner">
            <CaptureBanner platform={platform} down={down} onOpenNetwork={() => setShowSettings("network")} onError={onError} />
          </div>
        )}
        {live?.failsafe && live.failsafe.at !== failsafeSeen && !live.session && (
          <div className="main-banner">
            <FailsafeBanner failsafe={live.failsafe} onDismiss={() => dismissFailsafe(live.failsafe?.at ?? "")} />
          </div>
        )}
        {live?.line?.active && (
          <div className="main-banner">
            <LineBanner line={live.line} />
          </div>
        )}
        {game && live ? (
          <>
            <GamePanel
              key={game.id}
              game={game}
              live={live}
              settings={settings}
              relays={relays}
              demo={boot.demo}
              history={history}
              now={now}
              otherGameName={sessGame && sessGame.id !== game.id ? sessGame.name : null}
              onError={onError}
              onOpenRelays={() => setShowSettings("relays")}
              onEnableLoopback={enableLoopback}
            />
            {boot.demo && (
              <div style={{ maxWidth: 920, margin: "-22px auto 40px", padding: "0 28px" }}>
                <NetworkLab paths={live.snapshot?.paths ?? []} />
              </div>
            )}
          </>
        ) : (
          <Home games={games} recent={recent} demo={boot.demo} onSelect={selectGame} onAddGame={() => setShowAddGame(true)} />
        )}
      </main>

      {showSettings && (
        <SettingsModal
          initialTab={showSettings}
          settings={settings}
          relays={relays}
          platform={platform}
          version={boot.version}
          demo={boot.demo}
          onChange={setSettings}
          onRelaysChanged={refreshRelays}
          onRemap={() => {
            setShowSettings(null);
            setPhase("mapping");
          }}
          onClose={() => setShowSettings(null)}
          onError={onError}
        />
      )}
      {showAddGame && <AddGameModal games={games} onAdded={onGameAdded} onClose={() => setShowAddGame(false)} />}
      <Toasts toasts={toasts} onDismiss={(id) => setToasts((t) => t.filter((x) => x.id !== id))} />
    </div>
  );
}
