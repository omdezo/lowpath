import { ShieldCheck, X } from "lucide-react";
import type { Failsafe } from "../types";

// Shown after a boost ended without the user asking: the capture failed
// open during this run ("stopped"), or the last run was closed or crashed
// mid-boost and this start cleaned up ("recovered"). The message comes from
// the service; the game is on the normal connection either way.
export function FailsafeBanner({ failsafe, onDismiss }: { failsafe: Failsafe; onDismiss: () => void }) {
  const title = failsafe.kind === "recovered" ? "Lowpath closed during a boost" : "Boost stopped to keep your game connected";
  return (
    <div className="banner banner-warn" role="alert" aria-label="Boost ended">
      <ShieldCheck size={16} style={{ flex: "none" }} />
      <span style={{ flex: 1 }}>
        <b>{title}.</b> {failsafe.message}
        {failsafe.detail && <span className="banner-detail"> {failsafe.detail}</span>} Press BOOST to start again.
      </span>
      <button className="icon-btn" onClick={onDismiss} aria-label="Dismiss" title="Dismiss" style={{ flex: "none" }}>
        <X size={14} />
      </button>
    </div>
  );
}
