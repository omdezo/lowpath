package detect

import "path/filepath"

// Locations are the places a system keeps games and their launchers' notes
// about them. On Windows only Steam is looked up so far; on Linux and macOS
// a Windows game usually runs under Wine or Proton, so its files sit in a
// Wine prefix (Steam Play, Lutris, Heroic, Bottles, CrossOver, Whisky, a
// plain ~/.wine) and launchers record where they put a game.
type Locations struct {
	// Steam are Steam client folders (each with steamapps/ and appcache/).
	Steam []string
	// Prefixes are glob patterns (filepath.Glob) of Wine prefix folders,
	// the folders that hold drive_c. Proton's prefixes are found from the
	// Steam libraries and need no entry here.
	Prefixes []string
	// Lutris are folders of Lutris game files (<game>.yml), whose
	// "prefix:" lines name each game's Wine prefix. Lutris's pga.db is
	// not read (see lutrisPrefixes).
	Lutris []string
	// Heroic are Heroic's GamesConfig folders: <app>.json files whose
	// "winePrefix" names each game's Wine prefix.
	Heroic []string
	// Legendary are installed.json files of Legendary, the Epic Games
	// client Heroic runs: each game's "install_path".
	Legendary []string
	// GOG are Heroic's gog_store/installed.json files: each GOG game's
	// "install_path".
	GOG []string
	// Nile are installed.json files of Nile, the Amazon Games client
	// Heroic runs: each game's "path".
	Nile []string
}

// withHeroic adds the files Heroic keeps in each of its config folders
// (paths.ts and constants.ts: GamesConfig, and the installed lists of the
// Epic, GOG and Amazon clients it runs).
func (l Locations) withHeroic(roots ...string) Locations {
	j := filepath.Join
	for _, r := range roots {
		l.Heroic = append(l.Heroic, j(r, "GamesConfig"))
		l.Legendary = append(l.Legendary, j(r, "legendaryConfig", "legendary", "installed.json"))
		l.GOG = append(l.GOG, j(r, "gog_store", "installed.json"))
		l.Nile = append(l.Nile, j(r, "nile_config", "nile", "installed.json"))
	}
	return l
}

// UnixLocations are the usual Linux or macOS places (goos "linux" or
// "darwin") under the home folder home; env reads XDG_CONFIG_HOME and
// XDG_DATA_HOME on Linux. Folders that don't exist are simply not found
// later. No home folder means no locations.
//
// Sources: Steam's Linux install (~/.steam/steam links to
// ~/.local/share/Steam), its Flatpak (com.valvesoftware.Steam) and Snap
// packages, Heroic's paths.ts, legendary/constants.ts, the gog_store
// electron-store and nile_config (its Flatpak, com.heroicgameslauncher.hgl,
// keeps the same files under ~/.var/app), Lutris and Bottles defaults,
// CrossOver's and Whisky's bottle folders.
func UnixLocations(goos, home string, env func(string) string) Locations {
	if home == "" {
		return Locations{}
	}
	j := filepath.Join
	heroicPrefixes := []string{j(home, "Games", "Heroic", "Prefixes", "*"), j(home, "Games", "Heroic", "Prefixes", "*", "*")}
	switch goos {
	case "darwin":
		support := j(home, "Library", "Application Support")
		return Locations{
			Steam: []string{j(support, "Steam")},
			Prefixes: append([]string{
				j(home, ".wine"),
				j(support, "CrossOver", "Bottles", "*"),
				j(home, "Library", "Containers", "com.isaacmarovitz.Whisky", "Bottles", "*"),
			}, heroicPrefixes...),
			Legendary: []string{j(home, ".config", "legendary", "installed.json")},
		}.withHeroic(j(support, "heroic"))
	case "linux":
		config, data := j(home, ".config"), j(home, ".local", "share")
		if env != nil {
			if v := env("XDG_CONFIG_HOME"); filepath.IsAbs(v) {
				config = v
			}
			if v := env("XDG_DATA_HOME"); filepath.IsAbs(v) {
				data = v
			}
		}
		flatpak := func(app string, parts ...string) string {
			return j(append([]string{home, ".var", "app", app}, parts...)...)
		}
		return Locations{
			Steam: []string{
				j(home, ".steam", "steam"),
				j(home, ".steam", "root"),
				j(data, "Steam"),
				flatpak("com.valvesoftware.Steam", ".local", "share", "Steam"),
				flatpak("com.valvesoftware.Steam", "data", "Steam"),
				j(home, "snap", "steam", "common", ".local", "share", "Steam"),
			},
			Prefixes: append([]string{
				j(home, ".wine"),
				j(home, "Games", "*"), // Lutris installs each game, prefix included, here
				j(data, "bottles", "bottles", "*"),
				flatpak("com.usebottles.bottles", "data", "bottles", "bottles", "*"),
			}, heroicPrefixes...),
			Lutris: []string{
				j(config, "lutris", "games"),
				j(data, "lutris", "games"),
				flatpak("net.lutris.Lutris", "config", "lutris", "games"),
				flatpak("net.lutris.Lutris", "data", "lutris", "games"),
			},
			Legendary: []string{j(config, "legendary", "installed.json")},
		}.withHeroic(j(config, "heroic"), flatpak("com.heroicgameslauncher.hgl", "config", "heroic"))
	}
	return Locations{}
}
