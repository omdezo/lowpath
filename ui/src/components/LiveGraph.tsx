export interface Point {
  t: number; // ms epoch
  before: number | null;
  after: number | null;
}

const W = 800;
const H = 150;
const PAD_L = 34;
const PAD_B = 18;
const WINDOW_MS = 60_000;

function niceMax(v: number): number {
  const steps = [40, 60, 80, 100, 120, 150, 200, 250, 300, 400, 500, 750, 1000];
  return steps.find((s) => s >= v) ?? Math.ceil(v / 500) * 500;
}

function line(points: Point[], key: "before" | "after", x: (t: number) => number, y: (v: number) => number): string {
  let d = "";
  let pen = false;
  for (const p of points) {
    const v = p[key];
    if (v == null) {
      pen = false;
      continue;
    }
    d += `${pen ? "L" : "M"}${x(p.t).toFixed(1)},${y(v).toFixed(1)}`;
    pen = true;
  }
  return d;
}

// A 60 s rolling chart: faint = direct route (before), bold = optimized (after).
export function LiveGraph({ points, now }: { points: Point[]; now: number }) {
  const visible = points.filter((p) => p.t >= now - WINDOW_MS);
  const peak = Math.max(30, ...visible.flatMap((p) => [p.before ?? 0, p.after ?? 0]));
  const top = niceMax(peak * 1.15);
  const x = (t: number) => PAD_L + ((t - (now - WINDOW_MS)) / WINDOW_MS) * (W - PAD_L);
  const y = (v: number) => (H - PAD_B) * (1 - Math.min(v, top) / top) + 2;
  const after = line(visible, "after", x, y);
  const before = line(visible, "before", x, y);
  const lastAfter = [...visible].reverse().find((p) => p.after != null);
  const area = (() => {
    const segs = visible.filter((p) => p.after != null);
    if (segs.length < 2) return "";
    return `${after}L${x(segs[segs.length - 1].t).toFixed(1)},${H - PAD_B}L${x(segs[0].t).toFixed(1)},${H - PAD_B}Z`;
  })();
  const grid = [0.25, 0.5, 0.75, 1].map((f) => Math.round(top * f));

  return (
    <div className="graph">
      <svg viewBox={`0 0 ${W} ${H}`} preserveAspectRatio="none" style={{ height: 150 }} role="img" aria-label="Ping over the last 60 seconds">
        <defs>
          <linearGradient id="afterFill" x1="0" x2="0" y1="0" y2="1">
            <stop offset="0" stopColor="var(--accent)" stopOpacity="0.28" />
            <stop offset="1" stopColor="var(--accent)" stopOpacity="0" />
          </linearGradient>
        </defs>
        {grid.map((g) => (
          <g key={g}>
            <line x1={PAD_L} x2={W} y1={y(g)} y2={y(g)} stroke="var(--border)" strokeDasharray="3 5" vectorEffect="non-scaling-stroke" />
            <text x={PAD_L - 6} y={y(g) + 4} textAnchor="end" fontSize="11" fill="var(--faint)" className="num">
              {g}
            </text>
          </g>
        ))}
        <line x1={PAD_L} x2={W} y1={H - PAD_B} y2={H - PAD_B} stroke="var(--border-strong)" vectorEffect="non-scaling-stroke" />
        {[60, 45, 30, 15].map((s) => (
          <text key={s} x={x(now - s * 1000)} y={H - 4} fontSize="11" fill="var(--faint)" textAnchor="middle">
            -{s}s
          </text>
        ))}
        {area && <path d={area} fill="url(#afterFill)" />}
        <path d={before} fill="none" stroke="var(--graph-before)" strokeWidth="1.6" strokeDasharray="4 3" vectorEffect="non-scaling-stroke" strokeLinejoin="round" />
        <path d={after} fill="none" stroke="var(--accent)" strokeWidth="2.4" vectorEffect="non-scaling-stroke" strokeLinejoin="round" strokeLinecap="round" />
        {lastAfter && <circle cx={x(lastAfter.t)} cy={y(lastAfter.after!)} r="3.5" fill="var(--accent)" />}
      </svg>
    </div>
  );
}
