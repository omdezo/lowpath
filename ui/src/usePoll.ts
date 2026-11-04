import { useCallback, useEffect, useRef, useState } from "react";

export type Poll<T> = {
  /** The latest answer; null until the first one. */
  data: T | null;
  /** The latest ask failed; the last answer, if any, is kept. */
  failed: boolean;
  /** Shows an answer that came another way (a button's POST returns the new state). */
  set: (v: T) => void;
  /** Says the service didn't answer another way (that POST failed). */
  fail: () => void;
};

/**
 * usePoll asks `load` when the component mounts and then every `everyMs`,
 * counted from each answer so slow answers never pile up.
 *
 * It doesn't ask while the window is hidden (minimised, behind another app,
 * another tab): nobody sees the answers, and some asks keep work going in
 * the service. Each `GET /api/diag/gateway` keeps the router pinger running
 * for 15 s, so a Diagnostics tab left open would ping the router until the
 * window closed. When the window shows again it asks at once, so what is on
 * screen is never older than one interval.
 *
 * An answer that arrives after a newer one (an ask in flight while `set`
 * showed a POST's result) is dropped, so the card never steps back in time.
 */
export function usePoll<T>(load: () => Promise<T>, everyMs: number): Poll<T> {
  const [data, setData] = useState<T | null>(null);
  const [failed, setFailed] = useState(false);
  // The latest load, so a component may pass a new closure each render
  // without restarting the schedule.
  const loadRef = useRef(load);
  loadRef.current = load;
  const timer = useRef<number | undefined>(undefined);
  const alive = useRef(false);
  // asks counts asks: only the newest one schedules the next, so a second
  // ask (the window shown while one was in flight) never starts a second
  // schedule. shown counts set calls: an answer whose ask started before
  // one is older than what the card shows, and is dropped.
  const asks = useRef(0);
  const shown = useRef(0);

  const ask = useCallback(async () => {
    window.clearTimeout(timer.current);
    timer.current = undefined;
    const seq = ++asks.current;
    const gen = shown.current;
    let ok = true;
    let v: T | undefined;
    try {
      v = await loadRef.current();
    } catch {
      ok = false;
    }
    if (!alive.current || seq !== asks.current) return; // a newer ask owns the schedule
    if (gen === shown.current) {
      if (ok) {
        setData(v as T);
        setFailed(false);
      } else {
        setFailed(true);
      }
    }
    // Hidden: the next ask waits for the window to show (visibilitychange).
    if (!document.hidden) timer.current = window.setTimeout(ask, everyMs);
  }, [everyMs]);

  useEffect(() => {
    alive.current = true;
    const onVisibility = () => {
      if (document.hidden) {
        window.clearTimeout(timer.current);
        timer.current = undefined;
      } else if (timer.current === undefined) {
        ask();
      }
    };
    document.addEventListener("visibilitychange", onVisibility);
    if (!document.hidden) ask();
    return () => {
      alive.current = false;
      document.removeEventListener("visibilitychange", onVisibility);
      window.clearTimeout(timer.current);
      timer.current = undefined;
    };
  }, [ask]);

  const set = useCallback((v: T) => {
    shown.current++; // an ask already in flight is older than this
    setData(v);
    setFailed(false);
  }, []);
  const fail = useCallback(() => setFailed(true), []);

  return { data, failed, set, fail };
}
