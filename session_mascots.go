package main

// The ordered roster supplies automatic names and bundled terminal portraits.
// Colorscripts are vendored unchanged from pokemon-colorscripts; provenance and
// license are in THIRD_PARTY_NOTICES.txt. No runtime downloads or remote artwork
// are used. Only RGB foreground SGR and reset codes are allowed by asset tests.
// Portraits require a color-capable terminal; quiet, NO_COLOR, redirected output
// and TERM=dumb omit them. Client status and custom names remain plain-text usable.
// Keep the original eight names first to preserve automatic allocation order.
import (
	"embed"
	"strings"
)

//go:embed assets/pokemon-colorscripts/colorscripts/*.txt
var mascotFiles embed.FS

var sessionNames = strings.Fields("pikachu eevee bulbasaur charmander squirtle snorlax psyduck gengar meowth jigglypuff ditto mew vulpix raichu togepi pichu umbreon charizard dragonite lapras magikarp butterfree oddish cubone")

func automaticSessionNames() []string {
	return append([]string(nil), sessionNames...)
}

func sessionArt(name string) string {
	// Lookup only roster entries; names from a daemon are never file paths.
	for _, candidate := range sessionNames {
		if candidate == name {
			art, _ := mascotFiles.ReadFile("assets/pokemon-colorscripts/colorscripts/" + name + ".txt")
			return strings.Trim(string(art), "\n") + "\n"
		}
	}
	return ""
}
