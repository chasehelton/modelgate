package httpapi

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/chasehelton/modelgate/internal/rollout"
	"github.com/chasehelton/modelgate/internal/store"
)

// httptest.Server spins up a real listener on a random port. You are testing
// actual HTTP -- routing, status codes, JSON -- not calling handler funcs
// directly. This is the Go way to test an API.
func newTestServer(t *testing.T, seed bool) *httptest.Server {
	t.Helper()
	st := store.NewMemory()
	if seed {
		st.Seed([]rollout.Model{
			{ID: "gpt-5-mini", Percent: 25},
			{ID: "gpt-5-preview", Percent: 0},
		})
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := httptest.NewServer(New(st, log).Routes())
	t.Cleanup(srv.Close)
	return srv
}

func TestReadyzGatesOnStoreLoaded(t *testing.T) {
	unseeded := newTestServer(t, false)
	resp, err := http.Get(unseeded.URL + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("unseeded /readyz = %d, want 503", resp.StatusCode)
	}

	seeded := newTestServer(t, true)
	resp2, err := http.Get(seeded.URL + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("seeded /readyz = %d, want 200", resp2.StatusCode)
	}
}

// Liveness must NOT depend on the store, or a store outage makes K8s restart
// every pod and turns a degradation into a total outage.
func TestLivezIsShallow(t *testing.T) {
	srv := newTestServer(t, false)
	resp, err := http.Get(srv.URL + "/livez")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/livez = %d, want 200 even when not ready", resp.StatusCode)
	}
}

func TestAssignmentRequiresClientID(t *testing.T) {
	srv := newTestServer(t, true)
	resp, err := http.Get(srv.URL + "/v1/assignment")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing client_id = %d, want 400", resp.StatusCode)
	}
}

func TestRolloutLifecycle(t *testing.T) {
	srv := newTestServer(t, true)
	client := srv.Client()

	// Take gpt-5-preview to 100%: everyone should get it.
	req, _ := http.NewRequest(http.MethodPut,
		srv.URL+"/v1/models/gpt-5-preview/rollout",
		strings.NewReader(`{"percent":100}`))
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("set rollout = %d, want 200", resp.StatusCode)
	}

	if got := assignmentFor(t, srv.URL, "anyone"); got != "gpt-5-preview" {
		t.Fatalf("after 100%% rollout got %q, want gpt-5-preview", got)
	}

	// Kill switch: should immediately fall back.
	resp2, err := client.Post(srv.URL+"/v1/models/gpt-5-preview/disable", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()

	if got := assignmentFor(t, srv.URL, "anyone"); got == "gpt-5-preview" {
		t.Fatal("kill switch did not take effect")
	}
}

func TestSetRolloutValidation(t *testing.T) {
	srv := newTestServer(t, true)
	tests := []struct {
		name, path, body string
		wantStatus       int
	}{
		{"percent over 100", "/v1/models/gpt-5-mini/rollout", `{"percent":101}`, http.StatusBadRequest},
		{"negative percent", "/v1/models/gpt-5-mini/rollout", `{"percent":-1}`, http.StatusBadRequest},
		{"missing percent field", "/v1/models/gpt-5-mini/rollout", `{}`, http.StatusBadRequest},
		{"malformed json", "/v1/models/gpt-5-mini/rollout", `{nope`, http.StatusBadRequest},
		{"unknown model", "/v1/models/does-not-exist/rollout", `{"percent":10}`, http.StatusNotFound},
		{"percent zero is valid", "/v1/models/gpt-5-mini/rollout", `{"percent":0}`, http.StatusOK},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodPut, srv.URL+tc.path, strings.NewReader(tc.body))
			resp, err := srv.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.wantStatus)
			}
		})
	}
}

func TestMetricsExposesAssignments(t *testing.T) {
	srv := newTestServer(t, true)
	assignmentFor(t, srv.URL, "client-1")

	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "modelgate_assignments_total") {
		t.Fatalf("metrics missing assignments counter:\n%s", body)
	}
}

func assignmentFor(t *testing.T, base, clientID string) string {
	t.Helper()
	resp, err := http.Get(base + "/v1/assignment?client_id=" + clientID)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var a rollout.Assignment
	if err := json.NewDecoder(resp.Body).Decode(&a); err != nil {
		t.Fatal(err)
	}
	return a.Model
}
