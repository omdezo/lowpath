import { useEffect, useRef, useState } from "react";
import { Loader2, ShieldAlert } from "lucide-react";
import { api, describeError } from "../api";
import type { Platform } from "../types";

interface Props {
  platform: Platform;
  down: boolean;
  onOpenNetwork: () => void;
  onError: (msg: string, title?: string) => void;
}

// How long to wait for the elevated copy to take over before assuming the
// administrator prompt was cancelled.
const RESTART_GRACE_MS = 30_000;

// Route mode is selected but capture can't start in this process. Offers a
// restart with administrator rights when the platform can ask for them, and
// proxy mode otherwise. The server decides which (platform.can_elevate), so
// this component has no OS checks.
export function CaptureBanner({ platform, down, onOpenNetwork, onError }: Props) {
  const [state, setState] = useState<"idle" | "busy" | "restarting">("idle");
  const sawDown = useRef(false);
  const canElevate = platform.can_elevate && !platform.admin;

  useEffect(() => {
    if (down) sawDown.current = true;
  }, [down]);

  useEffect(() => {
    if (state !== "restarting") return;
    const t = window.setTimeout(() => {
      // If the service went away, the elevated copy is starting; the
      // bootstrap refresh after reconnect decides what to show.
      if (sawDown.current) return;
      setState("idle");
      onError("Lowpath is still running without administrator rights. If you cancelled the administrator prompt, try again.", "Not restarted");
    }, RESTART_GRACE_MS);
    return () => window.clearTimeout(t);
  }, [state, onError]);

  const elevate = async () => {
    setState("busy");
    sawDown.current = false;
    try {
      await api.elevate();
      setState("restarting");
    } catch (e) {
      setState("idle");
      const d = describeError(e, "Couldn't restart as administrator");
      onError(d.msg, d.title);
    }
  };

  const reason = platform.capture.reason || (canElevate ? "administrator rights are needed" : "the capture driver isn't available");

  return (
    <div className="banner banner-warn" role="alert">
      <ShieldAlert size={16} style={{ flex: "none" }} />
      <span style={{ flex: 1 }}>
        <b>Route mode can't capture game traffic:</b> {reason}.{" "}
        {canElevate ? (
          platform.service?.state === "not_installed" ? (
            <>
              Restart Lowpath as administrator to fix this, or install the background service once with{" "}
              <code className="inline-code">lowpathsvc install</code> so it never asks again.
            </>
          ) : (
            "Restart Lowpath as administrator to fix this."
          )
        ) : (
          <>
            Use{" "}
            <button className="link" onClick={onOpenNetwork}>
              Proxy mode
            </button>{" "}
            instead.
          </>
        )}
      </span>
      {canElevate && (
        <button className="btn btn-xs" onClick={elevate} disabled={state !== "idle"} style={{ flex: "none" }}>
          {state !== "idle" && <Loader2 size={13} className="spin" />}
          {state === "restarting" ? "Restarting…" : "Restart as administrator"}
        </button>
      )}
    </div>
  );
}
