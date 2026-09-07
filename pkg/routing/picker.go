package routing

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"math"
	"math/big"
	"sort"

	"github.com/sparksq/sparkroute/pkg/config"
)

// WeightedPicker returns a value in [0, totalWeight). Implementations must be
// safe for concurrent use because one routing snapshot serves many requests.
type WeightedPicker interface {
	Pick(totalWeight int64) (int64, error)
}

// CryptoPicker provides unbiased, process-independent weighted selection.
// Configuration changes do not require reseeding process-local PRNG state.
type CryptoPicker struct{}

func (CryptoPicker) Pick(totalWeight int64) (int64, error) {
	if totalWeight <= 0 {
		return 0, fmt.Errorf("total weight must be positive")
	}
	value, err := rand.Int(rand.Reader, big.NewInt(totalWeight))
	if err != nil {
		return 0, fmt.Errorf("random weighted selection: %w", err)
	}
	return value.Int64(), nil
}

var _ WeightedPicker = CryptoPicker{}

// weightedHashOrder returns a deterministic weighted rendezvous order. Each
// target receives an independent exponential-race score; sorting ascending
// provides weighted selection without replacement and minimal reassignment
// when targets are added or removed.
func weightedHashOrder(key string, targets []config.WeightedTarget) []int {
	type ranked struct {
		index int
		name  string
		score float64
	}
	rankedTargets := make([]ranked, len(targets))
	for index, target := range targets {
		digest := sha256.Sum256([]byte(key + "\x00" + target.Deployment))
		// Use 53 bits so the integer-to-float conversion remains exact. Neither
		// endpoint is zero, keeping Log defined for every digest.
		value := binary.BigEndian.Uint64(digest[:8]) >> 11
		unit := (float64(value) + 1) / (float64(uint64(1)<<53) + 1)
		rankedTargets[index] = ranked{
			index: index,
			name:  target.Deployment,
			score: -math.Log(unit) / float64(target.Weight),
		}
	}
	sort.Slice(rankedTargets, func(i, j int) bool {
		if rankedTargets[i].score == rankedTargets[j].score {
			return rankedTargets[i].name < rankedTargets[j].name
		}
		return rankedTargets[i].score < rankedTargets[j].score
	})
	order := make([]int, len(rankedTargets))
	for index, target := range rankedTargets {
		order[index] = target.index
	}
	return order
}
