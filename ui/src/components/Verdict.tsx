import { useCallback, useEffect, useRef, useState, type ReactNode } from "react";
import { ArrowRight, ChevronDown, ChevronUp, CircleAlert, CircleCheck, CircleDashed, Download, Stethoscope } from "lucide-react";
import { api } from "../api";
import type { CheckStatus, Confidence, Diagnosis, NextStep, RouteSegment, VerdictCheck, VerdictId } from "../types";
import { goToCard, isCardStep } from "./diagCards";
import "../diag.css";

// The verdict: one plain answer to "why am I lagging?" built from the tests
// below, the running boost and the last route analysis. It only reads what
// was measured, so it refreshes on its own as tests finish; a check with
// nothing to go on says "Not measured" and what to run, never a guess.

const POLL_MS = 4000;

type Tone = "good" | "warn" | "bad" | "none";

function verdictTone(id: VerdictId, c: Confidence): Tone {
  switch (id) {
    case "optimal":
    case "clean":
      return "good";
    case "unknown":
      return "none";
    default:
      return c === "low" ? "warn" : "bad";
  }
}

function checkTone(c: VerdictCheck): Tone {
  if (c.status === "unknown") return "none";
  if (c.rule === "optimal") return "good"; // either answer is good news
  return c.status === "found" ? "bad" : "good";
}

function checkLabel(c: VerdictCheck): string {
  const labels: Record<CheckStatus, string> = { found: "Problem", clear: "OK", unknown: "Not measured" };
  if (c.rule === "optimal" && c.status === "found") return "Not needed";
  if (c.rule === "optimal" && c.status === "clear") return "Relay helps";
  return labels[c.status];
}

const CONFIDENCE: Record<Confidence, string> = { high: "High confidence", medium: "Medium confidence", low: "Low confidence" };

// renderSegments draws the route part by part under the verdict (the
// Diagnostics tab passes its segment bar). withSteps: the tests the next
// steps name are on the same page, so each step can lead to its card.
export function Verdict({ renderSegments, withSteps }: { renderSegments?: (s: RouteSegment[]) => ReactNode; withSteps?: boolean } = {}) {
  const [d, setD] = useState<Diagnosis | null>(null);
  const [failed, setFailed] = useState(false);
  // The measurements under each check are long; they open on request.
  const [details, setDetails] = useState(false);
  const timer = useRef<number | undefined>(undefined);
  const alive = useRef(true);

  const poll = useCallback(async () => {
    window.clearTimeout(timer.current);
    try {
      const v = await api.verdict();
      if (!alive.current) return;
      setD(v);
      setFailed(false);
    } catch {
      if (alive.current) setFailed(true);
    }
    if (alive.current) timer.current = window.setTimeout(poll, POLL_MS);
  }, []);

  useEffect(() => {
    alive.current = true;
    poll();
    return () => {
      alive.current = false;
      window.clearTimeout(timer.current);
    };
  }, [poll]);

  const v = d?.verdict;
  // The first check that found something gave the verdict; its facts are
  // already shown under the verdict.
  const winner = d?.checks?.find((c) => c.status === "found")?.rule;
  const tone = v ? verdictTone(v.id, v.confidence) : "none";

  return (
    <section className="bloat verdict" aria-label="Verdict">
      <div className="bloat-head">
        <span className="bloat-ico">
          <Stethoscope size={18} />
        </span>
        <div className="txt">
          <div className="t">Why am I lagging?</div>
          <div className="d">Lowpath's verdict from the tests below, a running boost and the last route measurement. It updates as tests finish.</div>
        </div>
        <a className="btn" href="/api/diag/verdict/report" download="lowpath-diagnosis.txt" aria-disabled={!d}>
          <Download size={15} /> Export report
        </a>
      </div>

      {!d && !failed && <div className="bloat-empty muted">Reading the measurements…</div>}
      {!d && failed && <div className="note-warn bloat-note">— The verdict couldn't be loaded from the Lowpath service; it will try again.</div>}
      {d && failed && (
        <div className="note-warn bloat-note">The Lowpath service stopped answering, so this verdict may be out of date; it will try again.</div>
      )}

      {v && d && (
        <>
          <div className={`verdict-main t-${tone}`} role="status" aria-live="polite">
            <div className="verdict-title">
              <span className="t">{v.title}</span>
              <span className={`tag verdict-conf c-${v.confidence}`}>{CONFIDENCE[v.confidence]}</span>
              {v.lowpath_helps && <span className="tag tag-accent">Lowpath can help</span>}
            </div>
            <div className="verdict-detail">{v.detail}</div>
            {v.advice && (
              <div className="verdict-advice">
                <b>What to do:</b> {v.advice}
              </div>
            )}
            {v.facts && v.facts.length > 0 && (
              <ul className="verdict-facts" aria-label="Why">
                {v.facts.map((f, i) => (
                  <li key={i}>{f}</li>
                ))}
              </ul>
            )}
          </div>

          {renderSegments && d.segments && d.segments.length > 0 && renderSegments(d.segments)}

          <ul className="verdict-checks" aria-label="Checks">
            {(d.checks ?? []).map((c) => (
              <CheckRow key={c.rule} c={c} showFacts={details && c.rule !== winner} />
            ))}
          </ul>

          {d.next && d.next.length > 0 && <NextSteps steps={d.next} linked={!!withSteps} />}

          {(d.checks ?? []).some((c) => c.rule !== winner && c.facts && c.facts.length > 0) && (
            <button className="btn btn-ghost verdict-more" onClick={() => setDetails(!details)} aria-expanded={details}>
              {details ? <ChevronUp size={14} /> : <ChevronDown size={14} />} {details ? "Hide the measurements" : "Show the measurements behind each check"}
            </button>
          )}
        </>
      )}
    </section>
  );
}

// NextSteps lists what would make the verdict surer, one step per check
// left unmeasured. A step whose test is on this page leads to its card;
// measuring routes happens on a game's page, so that step is words only.
function NextSteps({ steps, linked }: { steps: NextStep[]; linked: boolean }) {
  return (
    <div className="verdict-next">
      <div className="verdict-next-t" id="verdict-next-t">
        For a surer answer
      </div>
      <ul aria-labelledby="verdict-next-t">
        {steps.map((s) => {
          const card = linked && isCardStep(s.id) ? s.id : null;
          return (
            <li key={s.id} data-step={s.id}>
              <span className="txt">{s.text}</span>
              {card && (
                <button className="btn btn-xs" onClick={() => goToCard(card)}>
                  {STEP_BUTTON[card]} <ArrowRight size={13} aria-hidden />
                </button>
              )}
            </li>
          );
        })}
      </ul>
    </div>
  );
}

const STEP_BUTTON = { bloat: "Bufferbloat test", trace: "Route trace" } as const;

function CheckRow({ c, showFacts }: { c: VerdictCheck; showFacts: boolean }) {
  const tone = checkTone(c);
  const Icon = tone === "none" ? CircleDashed : tone === "bad" ? CircleAlert : CircleCheck;
  return (
    <li className={`verdict-check k-${tone}`} data-rule={c.rule} data-status={c.status}>
      <Icon size={16} className="ico" aria-hidden />
      <div className="body">
        <div className="head">
          <span className="name">{c.name}</span>
          <span className="state">{checkLabel(c)}</span>
        </div>
        <div className="sum">{c.summary}</div>
        {showFacts && c.facts && c.facts.length > 0 && (
          <ul className="facts">
            {c.facts.map((f, i) => (
              <li key={i}>{f}</li>
            ))}
          </ul>
        )}
      </div>
    </li>
  );
}
