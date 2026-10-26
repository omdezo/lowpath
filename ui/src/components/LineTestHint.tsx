import { Gauge, X } from "lucide-react";

const DISMISSED_KEY = "lowpath.lineTestHint";

export function lineTestHintDismissed(): boolean {
  try {
    return localStorage.getItem(DISMISSED_KEY) === "1";
  } catch {
    return false;
  }
}

// Shown during a boost while no bufferbloat test has measured the line:
// the passive detector then compares other traffic with the most it saw
// in the last 10 minutes, a guess that can keep the "something is filling
// the connection" banner from showing. The test can't run during a boost,
// so this only points to it.
export function LineTestHint({ onOpen, onDismiss }: { onOpen: () => void; onDismiss: () => void }) {
  const dismiss = () => {
    try {
      localStorage.setItem(DISMISSED_KEY, "1");
    } catch {
      // Only a convenience; it still hides for this run.
    }
    onDismiss();
  };
  return (
    <div className="banner banner-info" role="status" aria-label="Bufferbloat test suggestion">
      <Gauge size={16} style={{ flex: "none" }} />
      <span style={{ flex: 1 }}>
        <b>Lowpath doesn't know your connection's speed yet.</b> It can warn you when a download elsewhere lags your game, but until the bufferbloat test
        has measured your line it only guesses from the traffic it sees. Run the test once in Diagnostics when you're not playing.
      </span>
      <button className="btn btn-sm" onClick={onOpen} style={{ flex: "none" }}>
        Open Diagnostics
      </button>
      <button className="btn btn-ghost icon-btn" onClick={dismiss} aria-label="Dismiss" title="Dismiss" style={{ flex: "none", width: 24, height: 24 }}>
        <X size={14} />
      </button>
    </div>
  );
}
