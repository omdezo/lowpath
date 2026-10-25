import { useCallback, useEffect, useMemo, useState, type FormEvent } from "react";
import { Loader2, Plus, RefreshCw, Search, X } from "lucide-react";
import { api, ApiError } from "../api";
import type { Game, ProcessInfo } from "../types";
import { Modal } from "./Bits";

interface Props {
  games: Game[];
  onAdded: (g: Game) => void;
  onClose: () => void;
}

// One row per executable: route mode captures every process with this image
// name, so several PIDs of the same program are one choice.
interface Program {
  name: string;
  pids: number[];
  ports: number[];
}

function group(list: ProcessInfo[]): Program[] {
  const by = new Map<string, Program>();
  for (const p of list) {
    if (!p.name) continue;
    const key = p.name.toLowerCase();
    const g = by.get(key) ?? { name: p.name, pids: [], ports: [] };
    g.pids.push(p.pid);
    for (const port of p.udp_ports ?? []) if (!g.ports.includes(port)) g.ports.push(port);
    by.set(key, g);
  }
  return [...by.values()].sort((a, b) => a.name.localeCompare(b.name, undefined, { sensitivity: "base" }));
}

// "VALORANT-Win64-Shipping.exe" → "VALORANT Win64 Shipping"
function displayName(exe: string): string {
  const base = exe.replace(/\.exe$/i, "").replace(/[_-]+/g, " ").trim();
  return base ? base.charAt(0).toUpperCase() + base.slice(1) : exe;
}

const listPorts = (ports: number[]) => {
  const s = [...ports].sort((a, b) => a - b);
  return s.length > 4 ? `${s.slice(0, 4).join(", ")} +${s.length - 4}` : s.join(", ");
};

export function AddGameModal({ games, onAdded, onClose }: Props) {
  const [programs, setPrograms] = useState<Program[] | null>(null);
  const [loading, setLoading] = useState(false);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [filter, setFilter] = useState("");
  const [picked, setPicked] = useState<string>("");
  const [name, setName] = useState("");
  const [nameEdited, setNameEdited] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const load = useCallback(async () => {
    setLoading(true);
    setLoadError(null);
    try {
      const r = await api.processes();
      setPrograms(group(r.processes ?? []));
    } catch (e) {
      setLoadError(e instanceof ApiError ? e.message : "Couldn't list running programs");
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    load();
  }, [load]);

  // Which library game already captures a given executable.
  const owner = useMemo(() => {
    const m = new Map<string, string>();
    for (const g of games) for (const p of g.processes ?? []) m.set(p.toLowerCase(), g.name);
    return m;
  }, [games]);

  const visible = useMemo(() => {
    const q = filter.trim().toLowerCase();
    return (programs ?? []).filter((p) => !q || p.name.toLowerCase().includes(q));
  }, [programs, filter]);

  const pick = (p: Program) => {
    setPicked(p.name);
    if (!nameEdited) setName(displayName(p.name));
    setError(null);
  };

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    if (!picked || !name.trim()) return;
    setBusy(true);
    setError(null);
    try {
      onAdded(await api.addCustomGame(name.trim(), picked));
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Couldn't add the game");
      setBusy(false);
    }
  };

  return (
    <Modal title="Add a game" onClose={onClose} className="add-game">
      <form onSubmit={submit} style={{ display: "contents" }}>
        <div className="modal-head">
          <div>
            <h2>Add a game</h2>
            <div className="muted" style={{ fontSize: 13, marginTop: 2 }}>
              Start the game first, then pick its program. Route mode captures every process with that name.
            </div>
          </div>
          <button type="button" className="btn btn-ghost icon-btn close" onClick={onClose} aria-label="Close">
            <X size={18} />
          </button>
        </div>

        <div className="modal-body">
          <div className="proc-tools">
            <label className="search" style={{ maxWidth: "none" }}>
              <Search size={16} />
              <input placeholder="Filter programs…" value={filter} onChange={(e) => setFilter(e.target.value)} aria-label="Filter programs" autoFocus />
            </label>
            <button type="button" className="btn" onClick={load} disabled={loading} aria-label="Refresh the program list">
              <RefreshCw size={15} className={loading ? "spin" : ""} /> Refresh
            </button>
          </div>

          <div className="proc-list" role="radiogroup" aria-label="Programs using UDP">
            {loadError ? (
              <div className="empty-list q-bad">{loadError}</div>
            ) : programs === null ? (
              <div className="empty-list">
                <Loader2 size={18} className="spin" />
              </div>
            ) : visible.length === 0 ? (
              <div className="empty-list">
                {programs.length === 0
                  ? "No running program has an open UDP socket. Start the game (join a match or open its server list) and refresh."
                  : `No program matches “${filter}”.`}
              </div>
            ) : (
              visible.map((p) => {
                const inLib = owner.get(p.name.toLowerCase());
                return (
                  <button type="button" key={p.name} className="proc-row" role="radio" aria-checked={picked === p.name} onClick={() => pick(p)}>
                    <span className="radio" />
                    <span className="meta">
                      <span className="name">{p.name}</span>
                      <span className="sub num">
                        PID {p.pids.join(", ")} · UDP {listPorts(p.ports) || "—"}
                        {inLib && <> · already in the library as {inLib}</>}
                      </span>
                    </span>
                  </button>
                );
              })
            )}
          </div>

          <div className="field">
            <label htmlFor="custom-game-name">Name in your library</label>
            <input
              id="custom-game-name"
              className="input"
              value={name}
              placeholder="Pick a program above"
              maxLength={60}
              onChange={(e) => {
                setName(e.target.value);
                setNameEdited(true);
              }}
            />
          </div>
          {error && (
            <div className="form-error" role="alert">
              {error}
            </div>
          )}
        </div>

        <div className="modal-foot">
          <button type="button" className="btn btn-ghost" onClick={onClose}>
            Cancel
          </button>
          <button type="submit" className="btn btn-primary" disabled={busy || !picked || !name.trim()}>
            {busy ? <Loader2 size={15} className="spin" /> : <Plus size={15} />}
            Add game
          </button>
        </div>
      </form>
    </Modal>
  );
}
