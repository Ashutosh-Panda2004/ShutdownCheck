package analyze

import (
	"sort"

	"github.com/Ashutosh-Panda2004/ShutdownCheck/pkg/schema"
)

// Deduction is one subtraction from a perfect score, kept alongside its reason
// so a score can always be explained rather than merely asserted.
type Deduction struct {
	ID     SignatureID
	Points int
	Reason string
}

// ScoreResult is the computed score with its full derivation.
type ScoreResult struct {
	Value      int
	Grade      string
	Deductions []Deduction
}

// Fixed weights, per spec section 9.2. They are published and versioned with
// the schema so any score can be reproduced and argued with.
const (
	weightInFlightDropped = 50
	weightSigtermIgnored  = 40
	weightSigkillRequired = 40
	weightAcceptNoAnswer  = 20
	weightListenerOpen    = 20
	weightNoDeregWindow   = 15
	weightReadinessStuck  = 15
	weightPortHeld        = 15
	weightAbruptReset     = 10
	weightEarlyExit       = 10
	weightMinor           = 5
)

// minorSignatures each cost the same small amount.
var minorSignatures = map[SignatureID]bool{
	SC008: true, SC009: true, SC013: true, SC014: true, SC016: true,
}

// Score computes the 0-100 shutdown score.
//
// It is informational: the verdict comes from rules, not from the score, unless
// the user sets an explicit minimum. Two signatures are scaled by how much of
// the traffic they actually affected, because losing every in-flight request is
// not the same defect as losing one.
func Score(f Facts, findings []Finding) ScoreResult {
	fired := map[SignatureID]bool{}
	for _, finding := range findings {
		fired[finding.ID] = true
	}

	var deductions []Deduction
	add := func(id SignatureID, points int, reason string) {
		if points <= 0 {
			return
		}
		deductions = append(deductions, Deduction{ID: id, Points: points, Reason: reason})
	}

	if fired[SC003] {
		ratio := f.InFlightDropRatio()
		add(SC003, scaled(weightInFlightDropped, ratio), "in-flight requests destroyed")
	}
	if fired[SC001] {
		add(SC001, weightSigtermIgnored, "the signal was ignored entirely")
	}
	if fired[SC002] {
		add(SC002, weightSigkillRequired, "a hard kill was required")
	}
	if fired[SC015] {
		add(SC015, weightAcceptNoAnswer, "connections accepted but never answered")
	}
	if fired[SC005] {
		add(SC005, scaled(weightListenerOpen, postWindowAcceptanceRatio(f)), "still accepting past the window")
	}
	if fired[SC006] {
		add(SC006, weightNoDeregWindow, "no de-registration window")
	}
	if fired[SC007] {
		add(SC007, weightReadinessStuck, "readiness never flipped")
	}
	if fired[SC012] {
		add(SC012, weightPortHeld, "port held after exit")
	}
	if fired[SC004] {
		add(SC004, scaled(weightAbruptReset, resetRatio(f)), "connections reset rather than closed")
	}
	if fired[SC011] {
		add(SC011, weightEarlyExit, "exited with work still pending")
	}

	for id := range minorSignatures {
		if fired[id] {
			info, _ := Lookup(id)
			add(id, weightMinor, info.Name)
		}
	}

	sort.Slice(deductions, func(i, j int) bool {
		if deductions[i].Points != deductions[j].Points {
			return deductions[i].Points > deductions[j].Points
		}
		return deductions[i].ID < deductions[j].ID
	})

	value := 100
	for _, d := range deductions {
		value -= d.Points
	}
	if value < 0 {
		value = 0
	}

	return ScoreResult{Value: value, Grade: schema.Grade(value), Deductions: deductions}
}

// scaled applies a ratio to a weight, always costing at least one point so that
// a real defect never rounds away to nothing.
func scaled(weight int, ratio float64) int {
	if ratio <= 0 {
		return 1
	}
	if ratio > 1 {
		ratio = 1
	}

	points := int(float64(weight)*ratio + 0.5)
	if points < 1 {
		points = 1
	}
	return points
}

func postWindowAcceptanceRatio(f Facts) float64 {
	stats := f.Stat(PhasePostWindow)
	if stats.Count == 0 {
		// The listener probe saw acceptance even though no request landed there.
		return 1
	}
	return float64(stats.OK) / float64(stats.Count)
}

func resetRatio(f Facts) float64 {
	closed := f.Connections.ClosedByFIN + f.Connections.ClosedByRST
	if closed == 0 {
		return 1
	}
	return float64(f.Connections.ResetAfterSignal) / float64(closed)
}
