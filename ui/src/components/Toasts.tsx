import { AlertTriangle, CheckCircle2, Repeat, X, Zap } from "lucide-react";

export interface Toast {
  id: number;
  kind: "applied" | "switch" | "degraded" | "fallback" | "recovered" | "error" | "success";
  msg: string;
  title?: string; // overrides the kind's default title
}

const META: Record<Toast["kind"], { title: string; color: string; Icon: typeof Zap }> = {
  applied: { title: "Routes applied", color: "var(--accent)", Icon: Zap },
  switch: { title: "Route switched", color: "var(--info)", Icon: Repeat },
  degraded: { title: "Route degraded", color: "var(--warn)", Icon: AlertTriangle },
  fallback: { title: "Routes down", color: "var(--bad)", Icon: AlertTriangle },
  recovered: { title: "Routes recovered", color: "var(--good)", Icon: CheckCircle2 },
  error: { title: "Something went wrong", color: "var(--bad)", Icon: AlertTriangle },
  success: { title: "Done", color: "var(--good)", Icon: CheckCircle2 },
};

export function Toasts({ toasts, onDismiss }: { toasts: Toast[]; onDismiss: (id: number) => void }) {
  return (
    <div className="toasts" aria-live="polite">
      {toasts.map((t) => {
        const m = META[t.kind];
        return (
          <div className="toast" key={t.id} role={t.kind === "fallback" || t.kind === "error" ? "alert" : "status"}>
            <m.Icon size={17} color={m.color} className="ico" />
            <div className="body">
              <div className="ttl">{t.title ?? m.title}</div>
              <div className="msg">{t.msg}</div>
            </div>
            <button className="btn btn-ghost icon-btn" style={{ width: 24, height: 24 }} onClick={() => onDismiss(t.id)} aria-label="Dismiss">
              <X size={14} />
            </button>
          </div>
        );
      })}
    </div>
  );
}
