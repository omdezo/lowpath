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
	// "prefix:" lines name each game's Wine prefix.
	Lutris []string
	// Heroic are Heroic's GamesConfig folders: <app>.json files whose
	// "winePrefix" names each game's Wine prefix.
	Heroic []string
	// Legendary are installed.json files of Legendary, the Epic Games
	// client Heroic runs: each game's "install_path".
	Legendary []string
}

// UnixLocations are the usual Linux or macOS places (goos "linux" or
// "darwin") under the home folder home; env reads XDG_CONFIG_HOME and
// XDG_DATA_HOME on Linux. Folders that don't exist are simply not found
// later. No home folder means no locations.
//
// Sources: Steam's Linux install (~/.steam/steam links to
// ~/.local/share/Steam), its Flatpak (com.valvesoftware.Steam) and Snap
// packages, Heroic's paths.ts and legendary/constants.ts, Lutris and
// Bottles defaults, CrossOver's and Whisky's bottle folders.
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
			Heroic: []string{j(support, "heroic", "GamesConfig")},
			Legendary: []string{
				j(support, "heroic", "legendaryConfig", "legendary", "installed.json"),
				j(home, ".config", "legendary", "installed.json"),
			},
		}
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
		heroicFlatpak := flatpak("com.heroicgameslauncher.hgl", "config", "heroic")
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
			Heroic: []string{j(config, "heroic", "GamesConfig"), j(heroicFlatpak, "GamesConfig")},
			Legendary: []string{
				j(config, "heroic", "legendaryConfig", "legendary", "installed.json"),
				j(heroicFlatpak, "legendaryConfig", "legendary", "installed.json"),
				j(config, "legendary", "installed.json"),
			},
		}
	}
	return Locations{}
}
