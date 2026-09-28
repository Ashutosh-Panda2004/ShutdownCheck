package analyze

import (
	"sort"

	"github.com/Ashutosh-Panda2004/ShutdownCheck/pkg/schema"
)

// Finding is a signature that fired, with the evidence that made it fire.
//
// Evidence is structured rather than prose so that a report can render it, a
// test can assert on it, and a user can argue with it. "6 of 22 in-flight
// requests failed" is checkable; "shutdown looked wrong" is not.
type Finding struct {
	ID       SignatureID
	Severity schema.Severity
	Summary  string
	Evidence map[string]any
}

// Signature detects one failure mode.
type Signature interface {
	ID() SignatureID
	Evaluate(Facts, Policy) (Finding, bool)
}

// rule adapts a plain function into a Signature. Each rule stays small enough
// to read in one go, which is the point: a verdict nobody can follow is a
// verdict nobody can trust.
type rule struct {
	id   SignatureID
	eval func(Facts, Policy) (summary string, evidence map[string]any, fired bool)
}

func (r rule) ID() SignatureID { return r.id }

func (r rule) Evaluate(f Facts, p Policy) (Finding, bool) {
	summary, evidence, fired := r.eval(f, p)
	if !fired {
		return Finding{}, false
	}
	return Finding{
		ID:       r.id,
		Severity: p.SeverityOf(r.id),
		Summary:  summary,
		Evidence: evidence,
	}, true
}

// Signatures returns every registered rule, ordered by identifier.
//
// The order is fixed because report output must be byte-identical across runs;
// ranging over a map would quietly break that.
func Signatures() []Signature {
	out := make([]Signature, len(registry))
	copy(out, registry)
	sort.Slice(out, func(i, j int) bool { return out[i].ID() < out[j].ID() })
	return out
}

// SignatureByID looks up a single rule.
func SignatureByID(id SignatureID) (Signature, bool) {
	for _, s := range registry {
		if s.ID() == id {
			return s, true
		}
	}
	return nil, false
}
