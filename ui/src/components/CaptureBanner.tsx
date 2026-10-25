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
// Windows prompt was cancelled.
const RESTART_GRACE_MS = 30_000;

// Route mode is selected but capture can't start in this process. Offers the
// one fix that works on Windows (run elevated) or, elsewhere, proxy mode.
export function CaptureBanner({ platform, down, onOpenNetwork, onError }: Props) {
  const [state, setState] = useState<"idle" | "busy" | "restarting">("idle");
  const sawDown = useRef(false);
  const canElevate = platform.os === "windows" && !platform.admin;

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
      onError("Lowpath is still running without administrator rights. If you cancelled the Windows prompt, try again.", "Not restarted");
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
          "Restart Lowpath as administrator to fix this."
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
