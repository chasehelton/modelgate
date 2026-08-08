// Package rollout contains the assignment logic: given a client ID and a set of
// models with rollout percentages, decide which model that client should use.
//
// The whole point of this package is STICKINESS. A client must get the same
// answer every time, and raising a rollout percentage must only ever ADD
// clients to the new model -- never move a client back off it.
package rollout

import (
	"hash/fnv"
	"sort"
)

// Model is a single servable model and its rollout state.
type Model struct {
	ID       string `json:"id"`
	Percent  int    `json:"percent"`  // 0..100, share of clients that should get it
	Disabled bool   `json:"disabled"` // kill switch: never serve this model
}

// Assignment is the answer we hand back to a client.
type Assignment struct {
	Model  string `json:"model"`
	Reason string `json:"reason"`
}

// DefaultModel is served when no rollout matches. Fail-open: a client always
// gets SOMETHING back, because returning an error here would break every
// Copilot request in the fleet. See docs/LEARNING.md "fail open vs fail closed".
const DefaultModel = "baseline"

// Bucket maps a client ID to a stable integer in [0,100).
//
// Properties that make this safe:
//   - Deterministic: same client_id always lands in the same bucket, with no
//     database lookup and no coordination between replicas.
//   - Uniform: FNV-1a spreads IDs evenly enough for percentage rollouts.
//   - Monotonic when combined with `bucket < percent`: going 25 -> 50 only ever
//     lets MORE buckets through. Nobody who was enrolled gets kicked out.
//
// Note the salt: bucketing every model off the same hash would mean the same
// unlucky clients are always in the first 1% of every single rollout. Salting
// per model decorrelates the cohorts.
func Bucket(clientID, salt string) int {
	h := fnv.New32a()
	_, _ = h.Write([]byte(salt))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(clientID))
	return int(h.Sum32() % 100)
}

// Assign picks a model for clientID.
//
// Models are evaluated in a stable order (sorted by ID) so that the result does
// not depend on map iteration order -- a classic Go footgun that would make
// assignments flap between replicas.
func Assign(clientID string, models []Model) Assignment {
	sorted := make([]Model, len(models))
	copy(sorted, models)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })

	for _, m := range sorted {
		if m.Disabled || m.Percent <= 0 {
			continue
		}
		if m.Percent >= 100 {
			return Assignment{Model: m.ID, Reason: "rollout:100%"}
		}
		if Bucket(clientID, m.ID) < m.Percent {
			return Assignment{Model: m.ID, Reason: "rollout:" + itoa(m.Percent) + "%"}
		}
	}
	return Assignment{Model: DefaultModel, Reason: "default"}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// TODO(exercise 1): add a per-model allowlist of client IDs that always get the
// model regardless of percentage (this is how real teams dogfood internally).
//
// TODO(exercise 2): today the first matching model in sorted order wins, which
// means two models at 50% do NOT split the fleet evenly. Design a fix and write
// a test that proves the split. Think about what happens to stickiness.
