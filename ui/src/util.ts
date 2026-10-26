import type { Stats } from "./types";

export type Quality = "good" | "warn" | "bad";

export function quality(s?: Stats | null): Quality {
  if (!s || !s.up || s.samples === 0) return "bad";
  if (s.loss_pct >= 3 || s.jitter_ms >= 12 || s.rtt_ms >= 160) return "bad";
  if (s.loss_pct >= 1 || s.jitter_ms >= 5 || s.rtt_ms >= 90) return "warn";
  return "good";
}

// Same ranking as the engine (internal/engine/tracker.go Score).
export function score(s?: Stats): number {
  if (!s || !s.up || s.samples === 0) return Infinity;
  return s.rtt_ms + 3 * s.jitter_ms + 8 * s.loss_pct;
}

export const ms = (v: number) => Math.round(v).toString();
export const pct = (v: number) => (v < 10 ? v.toFixed(1) : Math.round(v).toString());

export function initials(name: string): string {
  const minor = new Set(["of", "the", "from", "and", "a"]);
  const words = name
    .replace(/[:\-]/g, " ")
    .split(/\s+/)
    .filter((w) => w && !minor.has(w.toLowerCase()));
  if (words.length === 1) return words[0].slice(0, 2).toUpperCase();
  return (words[0][0] + words[1][0]).toUpperCase();
}

// Measured values that may be unknown: "—" instead of a number.
export const fmtMs = (v: number | null | undefined) => (v == null ? "—" : v < 10 ? v.toFixed(1) : Math.round(v).toString());
export const fmtMbps = (v: number | null | undefined) => (v == null ? "—" : v < 10 ? v.toFixed(1) : Math.round(v).toString());

export const cap = (s: string) => s.charAt(0).toUpperCase() + s.slice(1);

export function since(iso: string, now = Date.now()): string {
  const s = Math.max(0, Math.floor((now - new Date(iso).getTime()) / 1000));
  const m = Math.floor(s / 60);
  const h = Math.floor(m / 60);
  if (h > 0) return `${h}h ${m % 60}m`;
  if (m > 0) return `${m}m ${s % 60}s`;
  return `${s}s`;
}
