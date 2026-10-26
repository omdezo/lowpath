import type { ReactNode } from "react";
import { ShieldCheck } from "lucide-react";
import type { Platform } from "../types";

// The background service (LowpathSvc) as the server sees it: whether this
// window goes through it, and if not, what to do about it. Hidden where the
// OS has no service support yet.
export function ServiceRow({ platform, demo }: { platform: Platform | null; demo: boolean }) {
  const svc = platform?.service;
  if (!svc || svc.state === "unsupported") return null;
  const view = describe(svc.state, svc.message, demo);
  return (
    <div className="setting" data-testid="service-row">
      <span className="muted">
        <ShieldCheck size={16} />
      </span>
      <div className="txt">
        <div className="t">
          Background service
          <span className={`tag ${view.tag}`}>{view.label}</span>
        </div>
        <div className="d">{view.text}</div>
      </div>
    </div>
  );
}

function describe(state: string, message: string | undefined, demo: boolean): { label: string; tag: string; text: ReactNode } {
  switch (state) {
    case "in_use":
      return {
        label: "In use",
        tag: "tag-accent",
        text: "Routing runs in the Lowpath service, which has the rights packet capture needs, so this window never asks for administrator rights, after a restart of the PC too.",
      };
    // For running and stopped, message is the window's own reason for not
    // using the service (it wouldn't start, didn't answer, -no-service).
    case "running":
      return {
        label: "Not used",
        tag: "",
        text: demo
          ? "The service is installed and running, but the demo runs its own simulated network in this window."
          : `The service is installed and running, but this window runs its own engine${message ? `: ${message}` : ""}.`,
      };
    case "stopped":
      return {
        label: "Stopped",
        tag: "",
        text: message
          ? `The service is installed but stopped, and this window runs its own engine: ${message}.`
          : "The service is installed but stopped. Lowpath starts it the next time it opens.",
      };
    case "not_installed":
      return {
        label: "Not installed",
        tag: "",
        text: (
          <>
            Without it, route mode needs Lowpath to run as administrator. Install the service once with <code className="inline-code">lowpathsvc install</code> (one
            administrator prompt) and Lowpath won't ask again.
          </>
        ),
      };
    default:
      return { label: "—", tag: "", text: `Couldn't ask Windows about the service${message ? `: ${message}` : ""}.` };
  }
}
