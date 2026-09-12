package crowding

import "fmt"

// Ensemble is the 12-member average, and the target after the trend filter.
type Ensemble struct {
	// TargetFracOfEquity is the position to hold, per asset and bar, as a
	// signed fraction of equity — the research package's "target". It is a
	// PRE-COST figure: no fee, no funding, no slippage has been taken off
	// it, which is why nothing here is called net.
	TargetFracOfEquity [][]float64
	// MeanMemberSignal is the average of the members' {−1, 0, +1} states,
	// so a multiple of 1/12 — the fixture's expected_signal.
	MeanMemberSignal [][]float64
	// MeanMemberScore is the average of the members' z-scores; NaN wherever
	// any member's window is not yet valid.
	MeanMemberScore [][]float64
}

// EnsembleTargets runs every member on the panel, averages their targets,
// signals and scores IN MEMBER ORDER, and applies the trend filter to the
// averaged target (never to each member) — nearby models are averaged
// before execution so opposing orders net out.
func EnsembleTargets(panel Panel, cfg Config, members []Member) (Ensemble, error) {
	if err := panel.Validate(); err != nil {
		return Ensemble{}, err
	}
	if err := cfg.Validate(); err != nil {
		return Ensemble{}, err
	}
	if len(members) == 0 {
		// Python's sum([]) / len([]) raises; a silent all-flat strategy
		// with NaN averages must not be the Go answer.
		return Ensemble{}, fmt.Errorf("crowding: no ensemble members")
	}
	n := panel.Bars()
	scores := map[int][][]float64{}
	for _, m := range members {
		if _, done := scores[m.LookbackBars]; !done {
			scores[m.LookbackBars] = CrowdingScores(panel, m.LookbackBars)
		}
	}
	sumTarget, sumSignal, sumScore := zeros2(n), zeros2(n), zeros2(n)
	for _, m := range members {
		score := scores[m.LookbackBars]
		signal, err := HysteresisSignal(score, m.EntryZ, m.ExitZ, cfg.Side)
		if err != nil {
			return Ensemble{}, err
		}
		target, err := CausalWeights(panel.Close, signal, cfg.TargetVolAnnualFrac, cfg.MaxGrossFrac, m.LookbackBars)
		if err != nil {
			return Ensemble{}, err
		}
		for a := 0; a < 2; a++ {
			for i := 0; i < n; i++ {
				sumTarget[a][i] += target[a][i]
				sumSignal[a][i] += signal[a][i]
				sumScore[a][i] += score[a][i]
			}
		}
	}
	count := float64(len(members))
	for a := 0; a < 2; a++ {
		for i := 0; i < n; i++ {
			sumTarget[a][i] /= count
			sumSignal[a][i] /= count
			sumScore[a][i] /= count
		}
	}
	out := Ensemble{TargetFracOfEquity: sumTarget, MeanMemberSignal: sumSignal, MeanMemberScore: sumScore}
	if cfg.UseTrendFilter {
		filtered, err := ApplyTrendConfirmation(panel.Close, sumTarget, cfg.TrendHorizonsDays, cfg.TrendRequiredVotes)
		if err != nil {
			return Ensemble{}, err
		}
		out.TargetFracOfEquity = filtered
	}
	return out, nil
}

func zeros2(n int) [][]float64 {
	return [][]float64{make([]float64, n), make([]float64, n)}
}
