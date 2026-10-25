import { useState, type FormEvent, type ReactNode } from "react";
import { Check, Copy, KeyRound, Loader2, Plus, Server, Trash2, Wifi } from "lucide-react";
import { api, ApiError } from "../api";
import type { RelayTest, RelayView, Settings } from "../types";
import { ms, pct } from "../util";
import { Toggle } from "./Bits";

const DEPLOY_CMD = "powershell -ExecutionPolicy Bypass -File deploy\\deploy-relay.ps1 -Server root@YOUR_VPS_IP";
const CONTINENTS = ["Europe", "North America", "South America", "Asia", "Middle East", "Oceania", "Africa"];

interface Props {
  relays: RelayView[];
  settings: Settings;
  onSettings: (patch: Partial<Settings>) => Promise<void>;
  onRelaysChanged: () => void;
}

type TestState = { busy: boolean; result?: RelayTest; error?: string };

// An egress relay is one whose traffic leaves from itself: either it names
// no egress or names its own id.
const isEgress = (r: RelayView) => !r.egress || r.egress === r.id;

export function RelaysPane({ relays, settings, onSettings, onRelaysChanged }: Props) {
  const [tests, setTests] = useState<Record<string, TestState>>({});
  const [deleting, setDeleting] = useState<string | null>(null);
  const [listError, setListError] = useState<string | null>(null);
  const [loopbackBusy, setLoopbackBusy] = useState(false);

  const test = async (id: string) => {
    setTests((t) => ({ ...t, [id]: { busy: true } }));
    try {
      const result = await api.testRelay({ id });
      setTests((t) => ({ ...t, [id]: { busy: false, result } }));
    } catch (e) {
      setTests((t) => ({ ...t, [id]: { busy: false, error: e instanceof ApiError ? e.message : "Test failed" } }));
    }
  };

  const remove = async (r: RelayView) => {
    if (!window.confirm(`Remove the relay “${r.label || r.addr}”?`)) return;
    setDeleting(r.id);
    setListError(null);
    try {
      await api.deleteRelay(r.id);
      onRelaysChanged();
    } catch (e) {
      setListError(e instanceof ApiError ? e.message : "Couldn't remove the relay");
    } finally {
      setDeleting(null);
    }
  };

  const setLoopback = async (on: boolean) => {
    setLoopbackBusy(true);
    try {
      await onSettings({ loopback_relay: on });
    } finally {
      setLoopbackBusy(false);
      onRelaysChanged();
    }
  };

  const byId = new Map(relays.map((r) => [r.id, r]));
  const egresses = relays.filter((r) => isEgress(r) && !r.builtin);

  return (
    <>
      <h3>Relays</h3>
      <div className="muted" style={{ fontSize: 12.5, marginBottom: 12 }}>
        Lowpath sends your game's packets through these servers. Every route to one game server must leave from the same egress relay, so hops that
        forward to an egress are grouped with it.
      </div>

      {listError && (
        <div className="form-error" role="alert" style={{ marginBottom: 10 }}>
          {listError}
        </div>
      )}

      <div className="relay-list" role="list" aria-label="Relays">
        {relays.length === 0 && <div className="empty-list" style={{ padding: "18px 12px" }}>No relays yet. Add one below or turn on the loopback test relay.</div>}
        {relays.map((r) => {
          const t = tests[r.id];
          const via = !isEgress(r) ? byId.get(r.egress) : undefined;
          return (
            <div className="relay-row" role="listitem" key={r.id}>
              <span className="relay-ico" aria-hidden="true">
                {r.builtin ? <Wifi size={16} /> : <Server size={16} />}
              </span>
              <div className="relay-txt">
                <div className="t">
                  {r.label || r.addr}
                  {r.builtin && <span className="tag">Built-in</span>}
                  <span className={`tag ${r.has_key ? "tag-accent" : ""}`} title={r.has_key ? "Packets carry this relay's access key" : "No access key (open relay)"}>
                    <KeyRound size={10} /> {r.has_key ? "Key" : "No key"}
                  </span>
                </div>
                <div className="d">
                  <span className="num">{r.addr}</span>
                  {[r.region, r.continent].filter(Boolean).length > 0 && <> · {[r.region, r.continent].filter(Boolean).join(", ")}</>}
                  {!isEgress(r) && <> · forwards to {via ? via.label || via.addr : r.egress}</>}
                </div>
                <TestResult state={t} />
              </div>
              <div className="relay-actions">
                <button className="btn btn-xs" onClick={() => test(r.id)} disabled={t?.busy} aria-label={`Test ${r.label || r.addr}`}>
                  {t?.busy ? <Loader2 size={13} className="spin" /> : null}
                  Test
                </button>
                {!r.builtin && (
                  <button className="btn btn-xs btn-ghost" onClick={() => remove(r)} disabled={deleting === r.id} aria-label={`Delete ${r.label || r.addr}`} title="Delete">
                    {deleting === r.id ? <Loader2 size={13} className="spin" /> : <Trash2 size={14} />}
                  </button>
                )}
              </div>
            </div>
          );
        })}
      </div>

      <div className="setting">
        <div className="txt">
          <div className="t">Use loopback test relay</div>
          <div className="d">
            Runs a relay inside Lowpath on 127.0.0.1. Your game's packets are captured, tunneled through it and put back into the game, which proves that
            capture works on this PC before you rent a server. It gives no speedup: the traffic still leaves from your own internet connection.
          </div>
        </div>
        <Toggle label="Use loopback test relay" checked={!!settings.loopback_relay} disabled={loopbackBusy} onChange={setLoopback} />
      </div>

      <AddRelayForm egresses={egresses} onAdded={onRelaysChanged} />

      <div className="help-box">
        <div className="t">Deploy your own relay</div>
        <div className="d">
          You need a Linux VPS (x64 or ARM) close to your game's servers and SSH access to it. In the folder that holds Lowpath.exe and the{" "}
          <span className="num">deploy</span> folder, run:
        </div>
        <CopyLine text={DEPLOY_CMD} />
        <div className="d">It installs the relay as a service and prints the address and key to paste into the form above.</div>
      </div>
    </>
  );
}

function TestResult({ state }: { state?: TestState }) {
  if (!state || state.busy) return null;
  if (state.error) return <div className="relay-test q-bad">{state.error}</div>;
  const r = state.result;
  if (!r) return null;
  return (
    <div className={`relay-test ${r.ok ? "q-good" : "q-bad"}`} role="status">
      {r.ok ? (
        <span className="num">
          {ms(r.rtt_ms)} ms · {pct(r.loss_pct)}% loss
        </span>
      ) : (
        "No reply"
      )}
      {r.message && <span className="muted"> · {r.message}</span>}
    </div>
  );
}

function AddRelayForm({ egresses, onAdded }: { egresses: RelayView[]; onAdded: () => void }) {
  const empty = { label: "", addr: "", key: "", region: "", continent: "", egress: "" };
  const [f, setF] = useState(empty);
  const [busy, setBusy] = useState<"add" | "test" | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [test, setTest] = useState<TestState | undefined>();
  const [added, setAdded] = useState(false);

  const field = (k: keyof typeof empty) => ({
    value: f[k],
    onChange: (e: { target: { value: string } }) => {
      setF((x) => ({ ...x, [k]: e.target.value }));
      setAdded(false);
    },
  });

  // host:port with a numeric port; IPv6 literals must be bracketed.
  const addrOk = /^(\[[0-9a-fA-F:.]+\]|[^\s:[\]]+):\d{1,5}$/.test(f.addr.trim());

  const runTest = async () => {
    setError(null);
    setBusy("test");
    setTest({ busy: true });
    try {
      setTest({ busy: false, result: await api.testRelay({ addr: f.addr.trim(), key: f.key.trim() }) });
    } catch (e) {
      setTest({ busy: false, error: e instanceof ApiError ? e.message : "Test failed" });
    } finally {
      setBusy(null);
    }
  };

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setError(null);
    if (!addrOk) return setError("Enter the address as host:port, for example 203.0.113.10:40000.");
    setBusy("add");
    try {
      await api.addRelay({
        label: f.label.trim() || f.addr.trim(),
        addr: f.addr.trim(),
        key: f.key.trim(),
        region: f.region.trim(),
        continent: f.continent.trim(),
        egress: f.egress,
      });
      setF(empty);
      setTest(undefined);
      setAdded(true);
      onAdded();
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Couldn't add the relay");
    } finally {
      setBusy(null);
    }
  };

  return (
    <form className="relay-form" onSubmit={submit} noValidate aria-label="Add a relay">
      <div className="t">Add a relay</div>
      {error && (
        <div className="form-error" role="alert">
          {error}
        </div>
      )}
      <div className="form-grid">
        <Field id="relay-label" label="Label">
          <input id="relay-label" className="input" placeholder="Frankfurt VPS" autoComplete="off" {...field("label")} />
        </Field>
        <Field id="relay-addr" label="Address (host:port)">
          <input id="relay-addr" className="input num" placeholder="203.0.113.10:40000" autoComplete="off" spellCheck={false} required aria-invalid={!!f.addr && !addrOk} {...field("addr")} />
        </Field>
        <Field id="relay-key" label="Access key">
          <input id="relay-key" className="input num" placeholder="From deploy-relay.ps1 (optional)" autoComplete="off" spellCheck={false} {...field("key")} />
        </Field>
        <Field id="relay-egress" label="Forwards to egress">
          <select id="relay-egress" className="input" {...field("egress")}>
            <option value="">This is an egress (exit) relay</option>
            {egresses.map((r) => (
              <option key={r.id} value={r.id}>
                {r.label || r.addr}
              </option>
            ))}
          </select>
        </Field>
        <Field id="relay-region" label="Region">
          <input id="relay-region" className="input" placeholder="EU-Central" autoComplete="off" {...field("region")} />
        </Field>
        <Field id="relay-continent" label="Continent">
          <input id="relay-continent" className="input" list="relay-continents" placeholder="Europe" autoComplete="off" {...field("continent")} />
          <datalist id="relay-continents">
            {CONTINENTS.map((c) => (
              <option key={c} value={c} />
            ))}
          </datalist>
        </Field>
      </div>
      <div className="relay-form-foot">
        <TestResult state={test} />
        {added && !test && (
          <span className="saved">
            <Check size={13} /> Relay added
          </span>
        )}
        <span className="spacer" />
        <button type="button" className="btn" onClick={runTest} disabled={!!busy || !addrOk}>
          {busy === "test" && <Loader2 size={15} className="spin" />}
          Test
        </button>
        <button type="submit" className="btn btn-primary" disabled={!!busy || !f.addr.trim()}>
          {busy === "add" ? <Loader2 size={15} className="spin" /> : <Plus size={15} />}
          Add relay
        </button>
      </div>
    </form>
  );
}

function Field({ id, label, children }: { id: string; label: string; children: ReactNode }) {
  return (
    <div className="field">
      <label htmlFor={id}>{label}</label>
      {children}
    </div>
  );
}

function CopyLine({ text }: { text: string }) {
  const [copied, setCopied] = useState(false);
  const copy = async () => {
    try {
      await navigator.clipboard.writeText(text);
      setCopied(true);
      window.setTimeout(() => setCopied(false), 1400);
    } catch {
      // Clipboard can be blocked; the command is selectable text anyway.
    }
  };
  return (
    <div className="copy-line">
      <code>{text}</code>
      <button type="button" className="btn btn-xs btn-ghost" onClick={copy} aria-label="Copy command">
        {copied ? <Check size={14} /> : <Copy size={14} />}
        {copied ? "Copied" : "Copy"}
      </button>
    </div>
  );
}
