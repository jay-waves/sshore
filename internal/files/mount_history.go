package files

import (
	"cmp"
	"math"
	"slices"
	"time"
)

const VisitHistoryLimit = 1024

// VisitHistory stores relative directory paths and their time-weighted visit counts.
type VisitHistory map[string]directoryVisit
type directoryVisit struct {
	Score float64
	Last  time.Time
}

func (v directoryVisit) weight(now time.Time) float64 {
	age := now.Sub(v.Last)
	if age < 0 || age > 30*24*time.Hour {
		return 0
	}
	return v.Score * math.Exp2(-age.Hours()/(7*24))
}
func (h VisitHistory) add(key string, weight float64, now time.Time) {
	h[key] = directoryVisit{h[key].weight(now) + weight, now}
	h.trim(now)
}
func (h VisitHistory) trim(now time.Time) {
	excess := len(h) - VisitHistoryLimit
	if excess <= 0 {
		return
	}
	type candidate struct {
		key    string
		weight float64
	}
	compare := func(a, b candidate) int {
		return cmp.Or(cmp.Compare(a.weight, b.weight), cmp.Compare(a.key, b.key))
	}
	if excess == 1 {
		// The live visit path can add at most one entry.
		var worst candidate
		first := true
		for key, visit := range h {
			entry := candidate{key, visit.weight(now)}
			if first || compare(entry, worst) < 0 {
				worst = entry
				first = false
			}
		}
		delete(h, worst.key)
		return
	}

	// A merge can exceed the cap by a full session's history. Score each
	// candidate once at the same instant and remove the lowest-ranked entries.
	ranked := make([]candidate, 0, len(h))
	for key, visit := range h {
		ranked = append(ranked, candidate{key, visit.weight(now)})
	}
	slices.SortFunc(ranked, compare)
	for _, entry := range ranked[:excess] {
		delete(h, entry.key)
	}
}

// Merge combines independent mount sessions at a common scoring time.
func (h VisitHistory) Merge(delta VisitHistory, now time.Time) {
	for k, v := range h {
		if v.weight(now) <= 0 {
			delete(h, k)
		}
	}
	for k, v := range delta {
		if v.weight(now) > 0 {
			previous := h[k]
			last := v.Last
			if previous.Last.After(last) {
				last = previous.Last
			}
			h[k] = directoryVisit{previous.weight(last) + v.weight(last), last}
		}
	}
	h.trim(now)
}
