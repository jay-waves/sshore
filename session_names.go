package main

// Automatic names retain their original allocation order; explicit names remain supported.
import "strings"

var sessionNames = strings.Fields("pikachu eevee bulbasaur charmander squirtle snorlax psyduck gengar meowth jigglypuff ditto mew vulpix raichu togepi pichu umbreon charizard dragonite lapras magikarp butterfree oddish cubone ivysaur venusaur charmeleon wartortle blastoise caterpie metapod weedle kakuna beedrill pidgey pidgeotto pidgeot rattata raticate spearow fearow ekans arbok sandshrew sandslash nidoran-f nidorina nidoqueen nidoran-m nidorino nidoking clefairy clefable ninetales wigglytuff zubat golbat gloom vileplume paras parasect venonat venomoth diglett dugtrio persian golduck mankey primeape growlithe arcanine poliwag poliwhirl poliwrath abra kadabra alakazam machop machoke machamp bellsprout weepinbell victreebel tentacool tentacruel geodude graveler golem ponyta rapidash slowpoke slowbro magnemite magneton farfetchd doduo dodrio seel dewgong grimer muk shellder cloyster gastly haunter onix drowzee hypno krabby kingler voltorb electrode exeggcute exeggutor marowak hitmonlee hitmonchan lickitung koffing weezing rhyhorn rhydon chansey tangela kangaskhan horsea seadra goldeen seaking staryu starmie mr-mime scyther jynx electabuzz magmar pinsir tauros gyarados vaporeon jolteon flareon porygon omanyte omastar kabuto kabutops aerodactyl articuno zapdos moltres dratini dragonair mewtwo")

func automaticSessionNames() []string {
	return append([]string(nil), sessionNames...)
}
