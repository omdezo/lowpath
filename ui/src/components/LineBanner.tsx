import { Activity } from "lucide-react";
import type { LineState } from "../types";
import { fmtMbps, fmtMs } from "../util";

const what: Record<NonNullable<LineState["direction"]>, string> = {
  download: "a download",
  upload: "an upload",
  both: "downloads and uploads",
};

// Shown during a boost when the passive detector sees other traffic fill
// the line while the game's ping rises.
export function LineBanner({ line }: { line: LineState }) {
  const rise = line.rtt_ms != null && line.baseline_ms != null ? Math.max(0, line.rtt_ms - line.baseline_ms) : null;
  const up = line.direction === "upload";
  const other = up ? line.other_up_mbps : line.other_down_mbps;
  const cap = up ? line.line_up_mbps : line.line_down_mbps;
  const top = line.top;
  const topMbps = top ? (up ? top.up_mbps : line.direction === "both" ? top.down_mbps + top.up_mbps : top.down_mbps) : null;
  return (
    <div className="banner banner-warn" role="alert">
      <Activity size={16} style={{ flex: "none" }} />
      <span style={{ flex: 1 }}>
        <b>Something on your network is filling the connection</b> ({what[line.direction ?? "download"]}): game ping is up{" "}
        {rise == null ? "" : <span className="num">+{fmtMs(rise)} ms</span>}
        {rise == null ? "" : " "}
        with other traffic at <span className="num">{fmtMbps(other)}</span>
        {cap != null && (
          <>
            {" "}
            of <span className="num">{fmtMbps(cap)}</span>
          </>
        )}{" "}
        Mbit/s.{" "}
        {top ? (
          <>
            Most of it is <b>{top.name}</b> on this PC (<span className="num">{fmtMbps(topMbps)}</span> Mbit/s): pause it, or downloads, cloud backups or streams on
            other devices on your network.
          </>
        ) : (
          "Pause game updates, downloads, cloud backups or streams on this PC or others on your network."
        )}
      </span>
    </div>
  );
}
