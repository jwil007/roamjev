package roam

import (
	"github.com/jwil007/roamjev/internal/config"
	"github.com/jwil007/roamjev/internal/wpac"
)

// ScoreBSS exposes roamctl's AP scoring so other deciders (roamjev's
// classic policy) score candidates with exactly the same function.
func ScoreBSS(b wpac.RichBSS, cfg *config.Config) int {
	return score(b, cfg).finalScore
}
