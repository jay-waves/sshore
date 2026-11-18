package files

import (
	"math"
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
	for len(h) > VisitHistoryLimit {
		worst := ""
		low := math.Inf(1)
		for k, v := range h {
			if w := v.weight(now); w < low || w == low && k < worst {
				worst, low = k, w
			}
		}
		delete(h, worst)
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
