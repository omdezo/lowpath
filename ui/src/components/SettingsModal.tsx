import { useState, type ReactNode } from "react";
import { Activity, AlertTriangle, Check, Cpu, Download, Gauge, Network, Server, SlidersHorizontal, Wrench, X } from "lucide-react";
import { api } from "../api";
import type { CaptureMode, Platform, RelayView, Settings } from "../types";
import { cap } from "../util";
import { Modal, Toggle } from "./Bits";
import { Connection } from "./Connection";
import { RelaysPane } from "./RelaysPane";
import { BloatTest } from "./BloatTest";
import { RouteTrace } from "./RouteTrace";

export type SettingsTab = "general" | "network" | "relays" | "diagnostics" | "optimization" | "advanced";

interface Props {
  initialTab?: SettingsTab;
  settings: Settings;
  relays: RelayView[];
  platform: Platform | null;
  version: string;
  demo: boolean;
  onChange: (s: Settings) => void;
  onRelaysChanged: () => void;
  onRemap: () => void;
  onClose: () => void;
  onError: (msg: string, title?: string) => void;
}

export function SettingsModal(props: Props) {
  const { initialTab, settings, relays, platform, version, demo, onChange, onRelaysChanged, onRemap, onClose, onError } = props;
  const [tab, setTab] = useState<SettingsTab>(initialTab ?? "network");
  const [saved, setSaved] = useState(false);

  const set = async (patch: Partial<Settings>) => {
    const next = { ...settings, ...patch };
    onChange(next); // optimistic
    try {
      onChange(await api.saveSettings(next));
      setSaved(true);
      window.setTimeout(() => setSaved(false), 1400);
    } catch {
      onChange(settings);
    }
  };

  const tabs: { id: SettingsTab; label: string; icon: ReactNode }[] = [
    { id: "general", label: "General", icon: <SlidersHorizontal size={16} /> },
    { id: "network", label: "Network", icon: <Network size={16} /> },
    { id: "relays", label: "Relays", icon: <Server size={16} /> },
    { id: "diagnostics", label: "Diagnostics", icon: <Activity size={16} /> },
    { id: "optimization", label: "Optimization", icon: <Gauge size={16} /> },
    { id: "advanced", label: "Advanced", icon: <Wrench size={16} /> },
  ];

  const captureMode: CaptureMode = settings.capture_mode ?? "proxy";

  return (
    <Modal title="Settings" onClose={onClose} className="settings">
      <div className="modal-head">
        <h2>Settings</h2>
        {saved && (
          <span className="saved" style={{ marginLeft: 12 }}>
            <Check size={13} /> Saved
          </span>
        )}
        <button className="btn btn-ghost icon-btn close" onClick={onClose} aria-label="Close settings">
          <X size={18} />
        </button>
      </div>
      <div className="settings-body">
        <nav className="settings-nav" role="tablist" aria-orientation="vertical">
          {tabs.map((t) => (
            <button key={t.id} role="tab" aria-selected={tab === t.id} onClick={() => setTab(t.id)}>
              {t.icon}
              {t.label}
            </button>
          ))}
        </nav>
        <div className="settings-pane" role="tabpanel" key={tab} aria-label={tabs.find((t) => t.id === tab)?.label}>
          {tab === "general" && (
            <>
              <h3>General</h3>
              <Row t="Theme" d="Dark is the default.">
                <div className="segmented">
                  {(["dark", "light"] as const).map((th) => (
                    <button key={th} role="radio" aria-checked={settings.theme === th} onClick={() => set({ theme: th })}>
                      {cap(th)}
                    </button>
                  ))}
                </div>
              </Row>
              <Row t="Launch on Windows start" d="Start Lowpath when you sign in." soon>
                <Toggle label="Launch on start" checked={false} disabled onChange={() => {}} />
              </Row>
              <Row t="Minimize to tray" d="Keep Lowpath running in the system tray when the window closes." soon>
                <Toggle label="Minimize to tray" checked={false} disabled onChange={() => {}} />
              </Row>
              <Row t="Language" d="English only for now." soon>
                <span className="chip">English</span>
              </Row>
            </>
          )}

          {tab === "network" && (
            <>
              <h3>Network</h3>
              <Connection />
              <div className="muted" style={{ fontSize: 12.5, marginTop: 10, marginBottom: 6 }}>
                Changes take effect the next time you press Apply Routes.
              </div>
              <div className="setting" style={{ flexDirection: "column", alignItems: "stretch", gap: 10 }}>
                <div className="txt">
                  <div className="t" id="capture-mode-label">
                    Capture mode
                  </div>
                  <div className="d">How the game's packets reach Lowpath.{demo ? " The demo's simulated game client always uses proxy mode." : ""}</div>
                </div>
                <div className="choices" role="radiogroup" aria-labelledby="capture-mode-label">
                  {CAPTURE_MODES.map((m) => (
                    <button key={m.id} className="choice" role="radio" aria-checked={captureMode === m.id} onClick={() => set({ capture_mode: m.id })}>
                      <span className="radio" />
                      <span>
                        <span className="t">
                          {m.title}
                          {m.id === "route" && <span className="tag tag-accent">Recommended</span>}
                        </span>
                        <span className="d">{m.desc(platform?.os)}</span>
                      </span>
                    </button>
                  ))}
                </div>
                {captureMode === "route" && platform && !platform.capture.available && !demo && (
                  <div className="note-warn">
                    <AlertTriangle size={14} />
                    <span>Route mode can't start right now: {platform.capture.reason || "the capture driver is unavailable"}.</span>
                  </div>
                )}
              </div>
              <Row t="Packet duplication" d="How many routes carry every packet. More copies = smoother, but uses more upload bandwidth. Auto picks 2–4 from measured loss.">
                <div className="segmented">
                  {([0, 1, 2, 4] as const).map((v) => (
                    <button key={v} role="radio" aria-checked={settings.dup_level === v} onClick={() => set({ dup_level: v })}>
                      {{ 0: "Auto", 1: "Off", 2: "Balanced", 4: "Max" }[v]}
                    </button>
                  ))}
                </div>
              </Row>
              <Row t="Auto-switch routes" d="Replace a route that degrades with a clearly better one, mid-session.">
                <Toggle label="Auto-switch routes" checked={settings.auto_switch} onChange={(v) => set({ auto_switch: v })} />
              </Row>
              <Row t="Kill switch" d="If every route goes down, block game traffic instead of falling back to your normal connection.">
                <Toggle label="Kill switch" checked={settings.kill_switch} onChange={(v) => set({ kill_switch: v })} />
              </Row>
              <Row t="Analyze when a game is selected" d="Probe all routes as soon as you pick a game.">
                <Toggle label="Auto analyze" checked={settings.auto_analyze} onChange={(v) => set({ auto_analyze: v })} />
              </Row>
              <Row t="Stop when the game closes" d="Tear routes down automatically once the game's process exits.">
                <Toggle label="Stop on game exit" checked={settings.stop_on_game_exit} onChange={(v) => set({ stop_on_game_exit: v })} />
              </Row>
            </>
          )}

          {tab === "relays" && <RelaysPane relays={relays} settings={settings} onSettings={set} onRelaysChanged={onRelaysChanged} />}

          {tab === "diagnostics" && (
            <>
              <h3>Diagnostics</h3>
              <div className="muted" style={{ fontSize: 12.5, marginBottom: 6 }}>
                Is the lag on your side of the connection? Run the tests on Wi-Fi and again on a cable to compare.
              </div>
              <BloatTest onError={onError} />
              <RouteTrace onError={onError} />
            </>
          )}

          {tab === "optimization" && (
            <>
              <h3>Optimization</h3>
              <Row t="Traffic Shaper" d="Prioritize game packets over downloads and streams on this PC." soon icon={<Gauge size={18} />}>
                <Toggle label="Traffic Shaper" checked={false} disabled onChange={() => {}} />
              </Row>
              <Row t="FPS Boost" d="Reversible OS tweaks: game priority, power plan, Game Mode." soon icon={<Cpu size={18} />}>
                <Toggle label="FPS Boost" checked={false} disabled onChange={() => {}} />
              </Row>
              <Row t="Multi-Internet" d="Send the game stream over two connections at once (e.g. Ethernet + 4G)." soon icon={<Network size={18} />}>
                <Toggle label="Multi-Internet" checked={false} disabled onChange={() => {}} />
              </Row>
            </>
          )}

          {tab === "advanced" && (
            <>
              <h3>Advanced</h3>
              <Row t="Diagnostics report" d="Settings, last analysis and live engine stats as JSON. Relay keys are left out.">
                <a className="btn" href="/api/diagnostics" download="lowpath-report.json">
                  <Download size={15} /> Export
                </a>
              </Row>
              <Row t="Re-map routes" d="Run the startup latency sweep to every relay again.">
                <button className="btn" onClick={onRemap}>
                  Re-map
                </button>
              </Row>
              <Row t="About" d={`Lowpath v${version} · ${demo ? "simulated demo network" : "configured relays"}`}>
                <span />
              </Row>
            </>
          )}
        </div>
      </div>
    </Modal>
  );
}

// desc takes the platform's OS: route mode needs different rights on each.
const CAPTURE_MODES: { id: CaptureMode; title: string; desc: (os?: string) => string }[] = [
  {
    id: "route",
    title: "Route mode",
    desc: (os) =>
      "Captures the game's traffic automatically, with nothing to change in the game. " +
      (os === "linux" ? "Needs root rights or the CAP_NET_ADMIN and CAP_NET_RAW capabilities." : "Needs administrator rights."),
  },
  {
    id: "proxy",
    title: "Proxy mode",
    desc: () => "Point the game at a local Lowpath port. Only works for games that let you type a server address.",
  },
];

function Row({ t, d, soon, icon, children }: { t: string; d: string; soon?: boolean; icon?: ReactNode; children: ReactNode }) {
  return (
    <div className="setting">
      {icon && <span className="muted">{icon}</span>}
      <div className="txt">
        <div className="t">
          {t}
          {soon && <span className="soon">Coming soon</span>}
        </div>
        <div className="d">{d}</div>
      </div>
      {children}
    </div>
  );
}
