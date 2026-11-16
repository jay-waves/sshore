package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Automatic names retain their original allocation order; explicit names remain supported.

var sessionNames = strings.Fields("pikachu eevee bulbasaur charmander squirtle snorlax psyduck gengar meowth jigglypuff ditto mew vulpix raichu togepi pichu umbreon charizard dragonite lapras magikarp butterfree oddish cubone ivysaur venusaur charmeleon wartortle blastoise caterpie metapod weedle kakuna beedrill pidgey pidgeotto pidgeot rattata raticate spearow fearow ekans arbok sandshrew sandslash nidoran-f nidorina nidoqueen nidoran-m nidorino nidoking clefairy clefable ninetales wigglytuff zubat golbat gloom vileplume paras parasect venonat venomoth diglett dugtrio persian golduck mankey primeape growlithe arcanine poliwag poliwhirl poliwrath abra kadabra alakazam machop machoke machamp bellsprout weepinbell victreebel tentacool tentacruel geodude graveler golem ponyta rapidash slowpoke slowbro magnemite magneton farfetchd doduo dodrio seel dewgong grimer muk shellder cloyster gastly haunter onix drowzee hypno krabby kingler voltorb electrode exeggcute exeggutor marowak hitmonlee hitmonchan lickitung koffing weezing rhyhorn rhydon chansey tangela kangaskhan horsea seadra goldeen seaking staryu starmie mr-mime scyther jynx electabuzz magmar pinsir tauros gyarados vaporeon jolteon flareon porygon omanyte omastar kabuto kabutops aerodactyl articuno zapdos moltres dratini dragonair mewtwo")

func automaticSessionNames() []string {
	return append([]string(nil), sessionNames...)
}

func validSession(name string) error {
	if len(name) < 1 || len(name) > 48 {
		return usage("name must contain 1–48 ASCII characters")
	}
	for i, c := range []byte(name) {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || i > 0 && (c == '_' || c == '-')) {
			return usage("invalid name: use letters, digits, _ and -")
		}
	}
	return nil
}

var errObjectNameInUse = errors.New("name is already in use")

type resourceNameReservation struct {
	name           string
	startup, owner *os.File
}

func (r *resourceNameReservation) Close() {
	if r.owner != nil {
		r.owner.Close()
	}
	r.startup.Close()
}

// The startup lock bridges the handoff from the creator to the worker's lifetime
// lock. Lock files stay in place so concurrent creators always lock the same inode.
func reserveResourceName(kind, requested string) (*resourceNameReservation, error) {
	return reserveResourceNameExcept(kind, requested, "")
}

func reserveResourceNameExcept(kind, requested, reviewID string) (*resourceNameReservation, error) {
	names := automaticSessionNames()
	if requested != "" {
		if err := validSession(requested); err != nil {
			return nil, err
		}
		names = []string{requested}
	}
	dir, err := stateDir("names")
	if err != nil {
		return nil, err
	}
	occupied, err := occupiedObjectNames(reviewID)
	if err != nil {
		return nil, err
	}
	for _, name := range names {
		if occupied[name] {
			if requested != "" {
				return nil, fmt.Errorf("%s: %w", name, errObjectNameInUse)
			}
			continue
		}
		var held []*os.File
		var lockErr error
		for _, suffix := range []string{".startup.lock", ".lock"} {
			path := filepath.Join(dir, name+suffix)
			f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
			if err != nil {
				for _, h := range held {
					h.Close()
				}
				return nil, err
			}
			held = append(held, f)
			if err = privateStatePath(path, false); err != nil {
				for _, h := range held {
					h.Close()
				}
				return nil, err
			}
			if lockErr = lockResourceState(f); lockErr != nil {
				break
			}
		}
		if lockErr == nil {
			current, err := occupiedObjectNames(reviewID)
			if err != nil || current[name] {
				for _, f := range held {
					f.Close()
				}
				if err != nil {
					return nil, err
				}
				if requested != "" {
					return nil, fmt.Errorf("%s: %w", name, errObjectNameInUse)
				}
				continue
			}
			return &resourceNameReservation{name: name, startup: held[0], owner: held[1]}, nil
		}
		for _, f := range held {
			f.Close()
		}
		if !resourceLockBusy(lockErr) {
			return nil, lockErr
		}
		if requested != "" {
			return nil, fmt.Errorf("%s/%s: %w", kind, name, errObjectNameInUse)
		}
	}
	return nil, fmt.Errorf("all automatic %s names are in use; cancel an existing task and retry", kind)
}

func lockObjectName(name string) (*os.File, error) {
	if err := validSession(name); err != nil {
		return nil, err
	}
	dir, err := stateDir("names")
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, name+".lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = privateStatePath(path, false); err == nil {
		err = lockResourceState(f)
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func occupiedObjectNames(exceptReview string) (map[string]bool, error) {
	names, err := occupiedShellNames()
	if err != nil {
		return nil, err
	}
	for _, kind := range []string{"mount", "forward"} {
		dir, err := resourceStateDir(kind)
		if err != nil {
			return nil, err
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			if !strings.HasSuffix(entry.Name(), ".lock") || strings.HasSuffix(entry.Name(), ".startup.lock") {
				continue
			}
			f, err := os.OpenFile(filepath.Join(dir, entry.Name()), os.O_RDWR, 0600)
			if err != nil {
				return nil, err
			}
			err = lockResourceState(f)
			f.Close()
			if resourceLockBusy(err) {
				names[strings.TrimSuffix(entry.Name(), ".lock")] = true
			} else if err != nil {
				return nil, err
			}
		}
	}
	running, err := collectRunningResources("")
	if err != nil {
		return nil, err
	}
	for _, r := range running {
		names[r.Name] = true
	}
	dir, err := approvalDir()
	if err != nil {
		return nil, err
	}
	pending, err := pendingApprovals(dir)
	if err != nil {
		return nil, err
	}
	for _, r := range pending {
		if r.ID != exceptReview {
			names[r.DisplayName()] = true
		}
	}
	return names, nil
}
