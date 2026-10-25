import { useState, type CSSProperties } from "react";
import type { Game } from "../types";
import { GameTile } from "./Bits";

export type ArtKind = "icon" | "cover" | "hero" | "logo" | "header";

// Art the service had none of; remembered for the session so each miss costs
// one request, not one per render.
const missing = new Set<string>();
const artKey = (id: string, kind: ArtKind) => `${id}/${kind}`;
export const artUrl = (id: string, kind: ArtKind) => `/api/art/${encodeURIComponent(id)}/${kind}`;
export const hasArt = (id: string, kind: ArtKind) => !missing.has(artKey(id, kind));

// ArtImg shows one piece of game art, or nothing (then onMissing runs, so
// the parent can show its fallback). It fades in once decoded.
export function ArtImg({ id, kind, className, style, onMissing }: { id: string; kind: ArtKind; className?: string; style?: CSSProperties; onMissing?: () => void }) {
  const [loaded, setLoaded] = useState(false);
  const [failed, setFailed] = useState(() => missing.has(artKey(id, kind)));
  if (failed) return null;
  return (
    <img
      key={artKey(id, kind)}
      src={artUrl(id, kind)}
      alt=""
      draggable={false}
      loading="lazy"
      decoding="async"
      className={`art ${loaded ? "art-in" : ""} ${className ?? ""}`}
      style={style}
      onLoad={() => setLoaded(true)}
      onError={() => {
        missing.add(artKey(id, kind));
        setFailed(true);
        onMissing?.();
      }}
    />
  );
}

// GameIcon is the small square picture of a game: the icon of its own
// program, else a crop of its Steam cover, else a letter tile.
export function GameIcon({ game, size = 32 }: { game: Pick<Game, "id" | "name" | "color">; size?: number }) {
  const order: ArtKind[] = ["icon", "cover"];
  const [i, setI] = useState(() => order.findIndex((k) => hasArt(game.id, k)));
  const radius = Math.round(size * 0.24);
  if (i < 0 || i >= order.length) return <GameTile name={game.name} color={game.color} size={size} />;
  const kind = order[i];
  return (
    <span className={`gicon gicon-${kind}`} style={{ width: size, height: size, borderRadius: radius }} aria-hidden="true">
      <ArtImg
        key={kind}
        id={game.id}
        kind={kind}
        onMissing={() => {
          const next = order.findIndex((k, j) => j > i && hasArt(game.id, k));
          setI(next < 0 ? order.length : next);
        }}
      />
    </span>
  );
}
