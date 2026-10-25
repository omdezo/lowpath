import type { ReactNode } from "react";
import { Gamepad2, Monitor, Server } from "lucide-react";

interface Props {
  on: boolean; // the game's traffic rides the relay route
  flowing: boolean; // packets are actually moving (not just armed)
  relayLabel: string;
  serverLabel: string;
  toRelay: number | null; // ms, you → relay
  relayToServer: number | null; // ms, relay → game server
  direct: number | null; // ms, your normal route
}

// Node positions in the 800×124 drawing.
const Y = 48;
const YOU = 70;
const RELAY = 400;
const SRV = 730;
const LOWPATH = `M${YOU},${Y} L${SRV},${Y}`;
const BACK = `M${SRV},${Y} L${YOU},${Y}`;
const DIRECT = `M${YOU},${Y + 20} C${YOU + 110},${Y + 72} ${SRV - 110},${Y + 72} ${SRV},${Y + 20}`;

const msText = (v: number | null) => (v == null ? "? ms" : `${Math.max(0, Math.round(v))} ms`);

// RouteMap draws the two ways a packet can travel: the relay route Lowpath
// uses (straight line through the relay) and the normal internet route
// (the dip underneath). Dots move along whichever one carries the game.
export function RouteMap({ on, flowing, relayLabel, serverLabel, toRelay, relayToServer, direct }: Props) {
  const label = on
    ? `Game traffic goes you → ${relayLabel} (${msText(toRelay)}) → ${serverLabel} (${msText(relayToServer)}). Direct route: ${msText(direct)}.`
    : `Game traffic takes your normal route (${msText(direct)}). Lowpath route via ${relayLabel}: ${msText(toRelay)} + ${msText(relayToServer)}.`;
  return (
    <div className={`routemap ${on ? "on" : ""}`}>
      <svg viewBox="0 0 800 124" role="img" aria-label={label}>
        <path d={DIRECT} className="rm-direct" />
        <path d={LOWPATH} className="rm-lowpath" />

        {on && flowing && (
          <>
            {[0, 0.9].map((b) => (
              <circle key={b} r="3" className="rm-pkt">
                <animateMotion dur="1.8s" begin={`${b}s`} repeatCount="indefinite" path={LOWPATH} />
              </circle>
            ))}
            {[0.45].map((b) => (
              <circle key={b} r="2.5" className="rm-pkt rm-pkt-back">
                <animateMotion dur="1.8s" begin={`${b}s`} repeatCount="indefinite" path={BACK} />
              </circle>
            ))}
          </>
        )}
        {!on && (
          <>
            {[0, 1.3].map((b) => (
              <circle key={b} r="2.5" className="rm-pkt-direct">
                <animateMotion dur="2.6s" begin={`${b}s`} repeatCount="indefinite" path={DIRECT} />
              </circle>
            ))}
          </>
        )}

        <text x={(YOU + RELAY) / 2} y={Y - 9} className="rm-leg">
          {msText(toRelay)}
        </text>
        <text x={(RELAY + SRV) / 2} y={Y - 9} className="rm-leg">
          {msText(relayToServer)}
        </text>
        <text x={RELAY} y={Y + 50} className="rm-direct-label">
          Normal route · {msText(direct)}
        </text>

        <Node x={YOU} name="You" icon={<Monitor x={-9} y={-9} width={18} height={18} />} />
        <Node x={RELAY} name={relayLabel} icon={<Server x={-9} y={-9} width={18} height={18} />} accent />
        <Node x={SRV} name={serverLabel} icon={<Gamepad2 x={-9} y={-9} width={18} height={18} />} />
      </svg>
    </div>
  );
}

function Node({ x, name, icon, accent }: { x: number; name: string; icon: ReactNode; accent?: boolean }) {
  return (
    <g transform={`translate(${x},${Y})`}>
      <circle r="20" className={`rm-node ${accent ? "rm-node-relay" : ""}`} />
      <g className="rm-icon">{icon}</g>
      <text y={-30} className="rm-name">
        {name.length > 24 ? name.slice(0, 23) + "…" : name}
      </text>
    </g>
  );
}
