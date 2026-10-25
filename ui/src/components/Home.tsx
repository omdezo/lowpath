import { useState, type CSSProperties } from "react";
import { Plus } from "lucide-react";
import type { Game } from "../types";
import { ArtImg, GameIcon, hasArt } from "./GameArt";

interface Props {
  games: Game[];
  recent: string[];
  demo: boolean;
  onSelect: (id: string) => void;
  onAddGame: () => void;
}

const MAX = 9;

// Home is what you see before picking a game: the games you're most likely
// to play right now (running, favorites, recent, installed), as covers.
export function Home({ games, recent, demo, onSelect, onAddGame }: Props) {
  const byId = new Map(games.map((g) => [g.id, g]));
  const ordered = [
    ...games.filter((g) => g.running),
    ...games.filter((g) => g.favorite),
    ...recent.map((id) => byId.get(id)).filter((g): g is Game => !!g),
    ...games.filter((g) => g.installed),
    ...games.filter((g) => g.steam_app), // then games we have artwork for
    ...games,
  ];
  const picks = [...new Map(ordered.map((g) => [g.id, g])).values()].slice(0, demo ? MAX + 1 : MAX);

  return (
    <div className="home">
      <div className="home-head">
        <h2>Your games</h2>
        <div className="muted">Pick a game, press Boost, play.</div>
      </div>
      <div className="covers">
        {picks.map((g) => (
          <Cover key={g.id} game={g} onClick={() => onSelect(g.id)} />
        ))}
        {!demo && (
          <button className="cover cover-add" onClick={onAddGame}>
            <span className="cover-art">
              <Plus size={26} />
            </span>
            <span className="cover-name">Add a game</span>
            <span className="cover-sub">Any program, while it runs</span>
          </button>
        )}
      </div>
    </div>
  );
}

function Cover({ game, onClick }: { game: Game; onClick: () => void }) {
  const [art, setArt] = useState(() => hasArt(game.id, "cover"));
  const status = game.running ? "Running" : game.installed ? "Installed" : game.genre;
  return (
    <button className="cover" onClick={onClick} style={{ "--game": game.color } as CSSProperties}>
      <span className={`cover-art ${art ? "" : "cover-plain"}`}>
        {art ? (
          <ArtImg id={game.id} kind="cover" onMissing={() => setArt(false)} />
        ) : (
          <>
            <GameIcon game={game} size={56} />
          </>
        )}
        {game.running && <span className="cover-live">● Running</span>}
      </span>
      <span className="cover-name">{game.name}</span>
      <span className="cover-sub">{status}</span>
    </button>
  );
}
