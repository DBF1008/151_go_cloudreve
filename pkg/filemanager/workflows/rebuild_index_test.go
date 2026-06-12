package workflows

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestDecideRebuildAction(t *testing.T) {
	const (
		policyA = 1
		policyB = 2
	)

	cases := []struct {
		name        string
		hasEntity   bool
		policyID    int
		extractable bool
		filter      []int
		want        rebuildDecision
	}{
		// No filter active: every file is in scope and gets at least a file-name index.
		{"no filter, entity + extractable -> extract", true, policyA, true, nil, decisionExtract},
		{"no filter, entity not extractable -> name only", true, policyA, false, nil, decisionNameOnly},
		{"no filter, no entity -> name only", false, 0, false, nil, decisionNameOnly},
		{"no filter, no entity ignores extractable flag -> name only", false, 0, true, nil, decisionNameOnly},
		{"empty (non-nil) filter behaves like no filter", true, policyA, true, []int{}, decisionExtract},

		// Filter active: only files whose primary entity belongs to a selected policy are processed.
		{"filter, entity in policy + extractable -> extract", true, policyA, true, []int{policyA}, decisionExtract},
		{"filter, entity in policy, not extractable -> name only", true, policyA, false, []int{policyA, policyB}, decisionNameOnly},
		{"filter, entity out of policy -> skip", true, policyB, true, []int{policyA}, decisionSkip},
		{"filter, no entity (no policy) -> skip", false, 0, false, []int{policyA}, decisionSkip},
		{"filter, no entity ignores extractable flag -> skip", false, 0, true, []int{policyA}, decisionSkip},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := decideRebuildAction(tc.hasEntity, tc.policyID, tc.extractable, tc.filter)
			assert.Equal(t, tc.want, got)
		})
	}
}
