package main

// Automatic names retain their original allocation order; explicit names remain supported.
import "strings"

var sessionNames = strings.Fields("pikachu eevee bulbasaur charmander squirtle snorlax psyduck gengar meowth jigglypuff ditto mew vulpix raichu togepi pichu umbreon charizard dragonite lapras magikarp butterfree oddish cubone")

func automaticSessionNames() []string {
	return append([]string(nil), sessionNames...)
}
