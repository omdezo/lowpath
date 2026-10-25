import { useState } from "react";
import { ChevronRight, FlaskConical } from "lucide-react";
import { api } from "../api";
import type { PathStatus } from "../types";

// Demo-only controls to break or heal simulated paths, so route degradation,
// auto-switching and the kill switch can be watched live.
export function NetworkLab({ paths }: { paths: PathStatus[] }) {
  const [state, setState] = useState<Record<string, string>>({});
  const act = async (id: string, action: "degrade" | "outage" | "restore") => {
    await api.impair(id, action);
    setState((s) => {
      if (id === "all") return Object.fromEntries([...paths.map((p) => [p.id, action]), ["direct", s.direct ?? "restore"]]);
      return { ...s, [id]: action };
    });
  };
  const rows: { id: string; label: string }[] = [{ id: "direct", label: "Your ISP (direct route)" }, ...paths.map((p) => ({ id: p.id, label: p.label }))];
  return (
    <details className="card lab">
      <summary>
        <ChevronRight size={15} className="chev" />
        <FlaskConical size={15} />
        Network lab
        <span className="faint" style={{ fontWeight: 500, fontSize: 12.5 }}>
          · demo only: break a simulated route and watch Lowpath react
        </span>
      </summary>
      <div className="lab-rows">
        {rows.map((r) => {
          const s = state[r.id];
          return (
            <div className="lab-row" key={r.id}>
              <span className="name">
                {r.label}
                {s && s !== "restore" && <span className={`chip ${s === "outage" ? "chip-bad" : "chip-warn"}`} style={{ height: 20, marginLeft: 8, fontSize: 11 }}>{s === "outage" ? "outage" : "degraded"}</span>}
              </span>
              <button className="btn btn-xs" onClick={() => act(r.id, "degrade")}>
                Degrade
              </button>
              <button className="btn btn-xs" onClick={() => act(r.id, "outage")}>
                Outage
              </button>
              <button className="btn btn-xs btn-ghost" onClick={() => act(r.id, "restore")}>
                Restore
              </button>
            </div>
          );
        })}
        <div className="lab-row" style={{ marginTop: 4 }}>
          <span className="name faint">All relay routes at once</span>
          <button className="btn btn-xs" onClick={() => act("all", "outage")}>
            Outage all
          </button>
          <button className="btn btn-xs btn-ghost" onClick={() => act("all", "restore")}>
            Restore all
          </button>
        </div>
      </div>
    </details>
  );
}
