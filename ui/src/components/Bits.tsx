import { useEffect, useRef, useState, type KeyboardEvent, type ReactNode } from "react";
import type { Status } from "../types";
import { initials } from "../util";

export function Logo({ size = 28 }: { size?: number }) {
  return (
    <svg width={size} height={size} viewBox="0 0 32 32" aria-hidden="true">
      <rect width="32" height="32" rx="9" fill="var(--accent-soft)" stroke="var(--accent)" strokeOpacity=".35" />
      <path d="M6 21.5c5 0 6.2-11 10-11s5 7.5 10 7.5" stroke="var(--accent)" strokeWidth="3" fill="none" strokeLinecap="round" />
      <circle cx="6" cy="21.5" r="2.2" fill="var(--accent)" />
      <circle cx="26" cy="18" r="2.2" fill="var(--accent)" />
    </svg>
  );
}

export function GameTile({ name, color, size = 34 }: { name: string; color: string; size?: number }) {
  return (
    <span className="tile" style={{ width: size, height: size, background: color, fontSize: size * 0.38, borderRadius: size * 0.27 }} aria-hidden="true">
      {initials(name)}
    </span>
  );
}

const statusText: Record<Status | "error", string> = {
  idle: "Idle",
  analyzing: "Analyzing",
  applying: "Applying",
  optimizing: "Optimizing",
  error: "Routes down",
};

export function StatusPill({ status }: { status: Status | "error" }) {
  return (
    <span className={`pill pill-${status}`} role="status">
      <span className="dot" />
      {statusText[status]}
    </span>
  );
}

export function Toggle({ checked, onChange, disabled, label }: { checked: boolean; onChange: (v: boolean) => void; disabled?: boolean; label: string }) {
  return (
    <span className="switch">
      <input type="checkbox" role="switch" aria-label={label} checked={checked} disabled={disabled} onChange={(e) => onChange(e.target.checked)} />
      <span />
    </span>
  );
}

const FOCUSABLE = 'button:not(:disabled), [href], input:not(:disabled), select:not(:disabled), textarea:not(:disabled), [tabindex]:not([tabindex="-1"])';

export function Modal({ title, onClose, className, children }: { title: string; onClose?: () => void; className?: string; children: ReactNode }) {
  const ref = useRef<HTMLDivElement>(null);
  const closeRef = useRef(onClose);
  closeRef.current = onClose;
  // Read during the first render: by the time effects run, an autoFocus
  // field inside the dialog has already taken focus.
  const [opener] = useState(() => document.activeElement as HTMLElement | null);

  // Move focus into the dialog and give it back to whatever opened it, so
  // keyboard users don't lose their place.
  useEffect(() => {
    const el = ref.current;
    if (el && !el.contains(document.activeElement)) el.focus();
    return () => {
      if (opener?.isConnected) opener.focus();
    };
  }, [opener]);

  const onKeyDown = (e: KeyboardEvent<HTMLDivElement>) => {
    if (e.key === "Escape" && closeRef.current) {
      e.stopPropagation();
      closeRef.current();
      return;
    }
    if (e.key !== "Tab" || !ref.current) return;
    // Keep Tab inside the dialog while it is open.
    const items = [...ref.current.querySelectorAll<HTMLElement>(FOCUSABLE)].filter((n) => n.offsetParent !== null);
    if (items.length === 0) return;
    const first = items[0];
    const last = items[items.length - 1];
    if (e.shiftKey && (document.activeElement === first || document.activeElement === ref.current)) {
      e.preventDefault();
      last.focus();
    } else if (!e.shiftKey && document.activeElement === last) {
      e.preventDefault();
      first.focus();
    }
  };

  return (
    <div className="backdrop" onMouseDown={(e) => e.target === e.currentTarget && onClose?.()}>
      <div ref={ref} className={`modal ${className ?? ""}`} role="dialog" aria-modal="true" aria-label={title} tabIndex={-1} onKeyDown={onKeyDown}>
        {children}
      </div>
    </div>
  );
}
