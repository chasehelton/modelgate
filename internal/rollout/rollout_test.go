package rollout

import (
	"fmt"
	"testing"
)

// Table-driven tests are THE idiomatic Go test pattern. One slice of cases, one
// loop, subtests via t.Run so failures name themselves. Interviewers look for
// this specifically.
func TestAssign(t *testing.T) {
	tests := []struct {
		name      string
		models    []Model
		clientID  string
		wantModel string
	}{
		{
			name:      "no models falls back to default",
			models:    nil,
			clientID:  "alice",
			wantModel: DefaultModel,
		},
		{
			name:      "zero percent serves nobody",
			models:    []Model{{ID: "gpt-5-mini", Percent: 0}},
			clientID:  "alice",
			wantModel: DefaultModel,
		},
		{
			name:      "hundred percent serves everybody",
			models:    []Model{{ID: "gpt-5-mini", Percent: 100}},
			clientID:  "alice",
			wantModel: "gpt-5-mini",
		},
		{
			name:      "kill switch beats a full rollout",
			models:    []Model{{ID: "gpt-5-mini", Percent: 100, Disabled: true}},
			clientID:  "alice",
			wantModel: DefaultModel,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Assign(tc.clientID, tc.models)
			if got.Model != tc.wantModel {
				t.Fatalf("Assign(%q) = %q, want %q", tc.clientID, got.Model, tc.wantModel)
			}
		})
	}
}

// Stickiness: the same client must get the same answer forever, or users would
// flap between models on every request.
func TestAssignIsDeterministic(t *testing.T) {
	models := []Model{{ID: "gpt-5-mini", Percent: 37}}
	first := Assign("client-42", models)
	for i := 0; i < 1000; i++ {
		if got := Assign("client-42", models); got != first {
			t.Fatalf("assignment flapped on iteration %d: %+v != %+v", i, got, first)
		}
	}
}

// THE important test. Ramping a rollout up must only ever ADD clients. If a
// client on the new model gets demoted when you go 25%% -> 50%%, you have a
// broken rollout system and you will find out during an incident.
func TestRampingUpNeverDemotesAnEnrolledClient(t *testing.T) {
	const clients = 5000
	enrolled := map[string]bool{}

	for percent := 0; percent <= 100; percent += 5 {
		models := []Model{{ID: "gpt-5-mini", Percent: percent}}
		for i := 0; i < clients; i++ {
			id := fmt.Sprintf("client-%d", i)
			got := Assign(id, models).Model == "gpt-5-mini"
			if enrolled[id] && !got {
				t.Fatalf("client %s was demoted at percent=%d", id, percent)
			}
			if got {
				enrolled[id] = true
			}
		}
	}

	if len(enrolled) != clients {
		t.Fatalf("at 100%% expected all %d clients enrolled, got %d", clients, len(enrolled))
	}
}

// The bucket distribution should be roughly uniform, otherwise "5%" is a lie.
func TestBucketDistributionIsRoughlyUniform(t *testing.T) {
	const clients = 20000
	const percent = 10
	hits := 0
	for i := 0; i < clients; i++ {
		if Bucket(fmt.Sprintf("client-%d", i), "gpt-5-mini") < percent {
			hits++
		}
	}
	ratio := float64(hits) / float64(clients)
	if ratio < 0.08 || ratio > 0.12 {
		t.Fatalf("expected ~10%% of clients in bucket, got %.2f%%", ratio*100)
	}
}

// Salting per model means the same unlucky clients aren't the guinea pigs for
// every rollout. Two models at the same percent should pick different cohorts.
func TestSaltDecorrelatesCohorts(t *testing.T) {
	same := 0
	const clients = 2000
	for i := 0; i < clients; i++ {
		id := fmt.Sprintf("client-%d", i)
		if (Bucket(id, "model-a") < 10) == (Bucket(id, "model-b") < 10) {
			same++
		}
	}
	// If the salt did nothing this would be 100%.
	if same == clients {
		t.Fatal("cohorts are perfectly correlated; the salt is not being applied")
	}
}
