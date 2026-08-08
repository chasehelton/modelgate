# LEARNING.md — Go + Kubernetes, via one real service

This is the pick-up-anywhere guide. Clone the repo on any machine, open this file, keep going.

**Background:** written for a TypeScript/React frontend engineer moving toward backend/infra work. It assumes you can program well and skips "what is a variable." It explains the things that are genuinely *different* in Go and in Kubernetes.

---

## Why this project

It mirrors what a model-lifecycle team actually does: get configuration out to a huge fleet of clients safely and gradually. Every concept you need for that job — API design, progressive rollout, health checks, graceful shutdown, CI/CD — shows up naturally rather than as a tutorial exercise.

---

## Setup on a new machine

```bash
# Go
curl -sSLO https://go.dev/dl/go1.24.4.linux-amd64.tar.gz     # or darwin-arm64 for a Mac
sudo rm -rf /usr/local/go && sudo tar -C /usr/local -xzf go1.24.4.linux-*.tar.gz
export PATH=/usr/local/go/bin:$PATH        # add to ~/.zshrc or ~/.bashrc

# kind (local Kubernetes in Docker) + kubectl
go install sigs.k8s.io/kind@latest
export PATH=$HOME/go/bin:$PATH

git clone https://github.com/chasehelton/modelgate.git && cd modelgate
go test ./...
```

**Note:** `go test -race` does not work on a Raspberry Pi / some arm64 Linux boxes — ThreadSanitizer errors with "unsupported VMA range." Not your bug. CI runs the race detector on amd64 for you.

---

## Part 1 — Go, for someone who knows TypeScript

### The mental model shift

TypeScript's type system describes values that already exist at runtime; it is erased when it compiles. Go's types are real at runtime, there is no `any` escape hatch, and the compiler refuses far more. The compiler is stricter, but the language is *much* smaller — you can hold all of Go in your head, which is not true of TS.

### Things that will trip you up

**Errors are values, not exceptions.** There is no `try/catch`. Functions return `(result, error)` and you check it every time.

```go
m, err := s.store.Get(id)
if err != nil {
    return err        // this pattern is ~30% of all Go code you will write
}
```

It feels tediously verbose for about a week, then you notice you can see every failure path directly in the code. Use `errors.Is(err, store.ErrNotFound)` to compare against sentinel errors — not `==`, because errors get wrapped.

**Zero values, not `undefined`.** Every declared variable is already valid: `0`, `""`, `false`, `nil`. There is no uninitialized state. This is why `{"percent": 0}` and `{}` decode identically into an `int` field — and why `internal/httpapi/server.go` uses `*int` for the percent field. That pointer is the *only* way to tell "the user set it to zero" from "the user didn't send it." Getting this wrong on a kill-switch-adjacent API is a real outage.

**Capitalization is visibility.** `Exported` is public, `unexported` is package-private. No keywords.

**Interfaces are implicit.** A type satisfies an interface by having the methods — no `implements` clause. `store.Memory` satisfies `store.Store` without ever naming it. Idiomatic Go defines interfaces where they are *consumed*, not where they are implemented.

**Goroutines and races.** `go doSomething()` starts a concurrent goroutine. Every HTTP request is already its own goroutine — which is exactly why `store.Memory` needs a mutex around its map. Unguarded concurrent map access in Go is a hard crash, not a subtle bug.

**`defer` runs at function exit.** Great for `mu.Unlock()` and `resp.Body.Close()`. Note it fires at *function* end, not block end.

### The stdlib is genuinely enough

This repo has **zero third-party dependencies**. Since Go 1.22 the stdlib router handles method+wildcard patterns:

```go
mux.HandleFunc("PUT /v1/models/{id}/rollout", handler)
id := r.PathValue("id")
```

Coming from npm this feels impossible. Reaching for gin/chi/echo in Go is now a choice, not a requirement — and "I used the stdlib and here's why" is a good interview answer.

### Testing is a first-class citizen

No Jest, no Vitest, no config. Files ending `_test.go`, functions starting `Test`. Two patterns to internalize:

**Table-driven tests** — one slice of cases, one loop, `t.Run` subtests. This is *the* idiomatic Go test and interviewers look for it specifically. See `TestSetRolloutValidation`.

**`httptest.Server`** — spins up a real listener on a random port so you test actual HTTP semantics (routing, status codes, JSON) rather than calling handler functions directly.

The most valuable test in this repo is `TestRampingUpNeverDemotesAnEnrolledClient`. It ramps 0→100% across 5,000 clients and fails if anyone is ever demoted. It tests a *property* of the system, not a hardcoded input/output pair. That kind of test is what separates "I wrote tests" from "I understand what could go wrong."

---

## Part 2 — Progressive delivery

The concept the whole job is about: **change is the leading cause of outages, so make change gradual and reversible.**

**Deterministic hash bucketing** (see README) gives stickiness and monotonicity with no state. Understand it well enough to derive it on a whiteboard.

**Ramp discipline.** 1% → 5% → 25% → 50% → 100%, with a bake period at each step long enough for problems to surface. The first step is small enough that if it's catastrophic, almost nobody saw it.

**Kill switch vs rollback.** Two different tools:
- *Kill switch* — config change, instant, no deploy, no CI. What you reach for at 3am.
- *Rollback* — `kubectl rollout undo`, replaces running code, takes a rollout cycle.

Config rollout should always be faster than code rollout. That's why the percentage lives in the store and not in a YAML file that needs a deploy.

**Fail open vs fail closed.** When the config system is broken, does the client get *nothing* (closed) or a *sensible default* (open)? Here: fail open to `baseline`, because a model router that returns 500 breaks every downstream client. For an auth service the answer is the opposite. Knowing that this is a *deliberate, context-dependent choice* — and being able to say which you picked and why — is a senior-engineer signal.

**Blast radius.** Ask "if this is wrong, how many people find out before I do?" Rings/canaries/percentages are all answers to that one question. Microsoft's ring deployment model is this idea, and you can speak to it from your own experience.

---

## Part 3 — Kubernetes, the parts that matter

Ignore 90% of K8s. These are the parts that actually come up.

### Objects

- **Pod** — one or more containers scheduled together. Cattle, not pets; assume it dies at any moment.
- **Deployment** — "keep N copies of this pod running, and update them per this strategy." What you actually write.
- **Service** — stable virtual IP + DNS name in front of a changing set of pods. Finds them by **label selector**. Typo the selector and you get zero endpoints and connection refused. Debug with `kubectl get endpoints modelgate`.
- **ConfigMap / Secret** — config and credentials injected as env vars or files.

### The probes (the highest-value thing here)

| Probe | Question | Failure action |
| --- | --- | --- |
| `startupProbe` | Has it finished booting? | Suspends the other two while running |
| `livenessProbe` | Is the process wedged? | **Restarts the container** |
| `readinessProbe` | Should it get traffic *right now*? | **Removes from Service endpoints** — no restart |

**The classic outage:** someone points `livenessProbe` at a handler that checks the database. The database has a blip. Every pod fails liveness simultaneously, K8s restarts the entire fleet, and a minor degradation becomes a total outage — with the restarts stampeding the recovering database.

**The rule:** liveness shallow, readiness deep. This repo enforces it — `/livez` returns 200 unconditionally, `/readyz` gates on the store being loaded. Verified: during an 8-second warmup, `/readyz` returned 503 for 5 polls while `/livez` returned 200 throughout.

**The other classic:** a readiness probe that never passes because of a typo'd path. With `maxUnavailable: 0`, this is *safe* — the rollout stalls, old pods keep serving, and you get a stuck deploy instead of an outage. Go break it deliberately in `deployment.yaml` and watch. That experience is worth more than reading about it.

### Rolling updates and graceful shutdown

`maxUnavailable: 0, maxSurge: 1` — bring up a new pod, wait for it to be *ready*, then retire an old one. Never dips below full capacity, and a bad build stalls instead of breaking.

But zero-downtime deploys need cooperation from your app. On SIGTERM the process must stop accepting new connections and let in-flight requests finish (`cmd/modelgate/main.go`). Without it, every deploy drops the requests that were mid-flight — a small, constant, maddening error rate that nobody can reproduce. Verified here: SIGTERM under continuous load drained 69 requests with **zero 5xx**.

Subtlety worth knowing (this is `TODO(exercise 6)`): K8s sends SIGTERM *and* removes the pod from Service endpoints at the same time, and those propagate at different speeds. So a pod can still receive new requests for a moment after SIGTERM. The standard fix is a short sleep *before* `Shutdown()` begins.

### Resources

`requests` = what the scheduler reserves. `limits` = the hard cap. Memory over-limit → **OOMKilled**. CPU over-limit → **throttled**, which shows up as mysterious latency even on an idle node. That's why this repo sets a memory limit but no CPU limit.

### Commands you'll actually use

```bash
kubectl get pods -w                       # watch a rollout live
kubectl rollout status deployment/modelgate
kubectl rollout undo deployment/modelgate
kubectl describe pod <name>               # Events at the bottom = why it's broken
kubectl logs -f deployment/modelgate
kubectl get endpoints modelgate           # empty = your selector is wrong
kubectl port-forward svc/modelgate 8080:80
```

`kubectl describe` → read the **Events** section first. It says `ImagePullBackOff` or `Readiness probe failed` in plain English 90% of the time.

### War story: the first CI run failed, and it's a good one

The very first push had all three pods stuck in `CreateContainerConfigError` and the rollout timed out. Worth understanding because it's a real class of bug, not a typo:

The Dockerfile declares `USER nonroot:nonroot` — a *name*. The Deployment sets `runAsNonRoot: true`, which makes the kubelet **verify the user isn't root before starting the container**. But the kubelet doesn't have the image's `/etc/passwd`, so it cannot resolve the name `nonroot` to a UID, so it can't prove the user isn't root — and it refuses to start rather than risk running as root.

The fix is a numeric UID in the pod's `securityContext`:

```yaml
securityContext:
  runAsNonRoot: true
  runAsUser: 65532     # the numeric uid behind distroless "nonroot"
```

Three lessons worth carrying into an interview:
1. **A working `docker run` does not mean a working pod.** The container ran fine locally; K8s applies admission and security constraints Docker never does.
2. **`CreateContainerConfigError` means the kubelet rejected the config before ever starting the container** — so `kubectl logs` is empty and useless. Go to `kubectl describe` → Events.
3. **This is exactly why CI runs a real cluster.** A manifest-lint job would have passed this YAML happily. Only actually applying it to a cluster caught it.

---

## Part 4 — Containers and CI

**Multi-stage builds.** Stage 1 compiles, stage 2 keeps only the binary. 14MB instead of ~800MB. In K8s, smaller images pull faster, so rollouts and scale-ups are faster.

**Distroless.** No shell, no package manager. RCE has nothing to exec. Tradeoff: you can't `kubectl exec` in to poke around — you debug via logs, metrics, and ephemeral debug containers. Being able to articulate that tradeoff is the point.

**Layer caching.** Copy `go.mod` and download deps *before* copying source, so dependency downloads aren't re-run on every code edit.

**Immutable tags.** Tag images with the commit SHA, not just `:latest`. Rollback becomes a precise operation instead of a guess.

The CI in `.github/workflows/ci.yml` gates on gofmt → vet → race tests → build, then spins up a **real kind cluster**, applies the manifests, and curls the API through the Service. That last part is what stops the manifests from silently rotting.

---

## The exercises

Numbered `TODO(exercise N)` markers in the code, roughly increasing in difficulty:

1. **Allowlist** (`rollout.go`) — clients that always get a model regardless of percentage. How real teams dogfood internally.
2. **Even splits** (`rollout.go`) — two models at 50% do *not* currently split the fleet evenly; first-in-sorted-order wins. Fix it, and write a test proving the split. Think hard about what it does to stickiness.
3. **Audit log** (`store.go`) — who changed what, when, old → new. Try running an incident postmortem without it.
4. **Postgres store** (`store.go`) — implement the `Store` interface. **If you have to touch `internal/httpapi` to make it work, the interface was wrong.** That's the real lesson.
5. **Latency histogram** (`server.go`) — you cannot compute a p99 from a counter. Pick bucket boundaries and justify them.
6. **Pre-shutdown delay** (`main.go`) — the endpoint-propagation race described above.

Then the K8s experiments:

7. Break the readiness probe path. Deploy. Watch the rollout stall while old pods keep serving. **Do this one.**
8. Set `STARTUP_DELAY_SECONDS: 30` with a 30s startup budget. Watch it fail, then fix the probe thresholds.
9. Run `kubectl rollout undo` and time how long a rollback actually takes.
10. Hammer the service with a curl loop during a rolling update. Confirm zero dropped requests. Then delete `srv.Shutdown()` and watch them drop.

---

## Interview prep from this repo

Rehearse these out loud:

- **"Walk me through a system you designed."** → modelgate. Problem, the hashing decision, failure modes, what you'd add for production. The README is structured as exactly this answer.
- **"Design a service that serves model config to 100M clients."** → This, scaled up: edge caching, propagation-delay budget, gradual rollout, blast-radius containment, fail-open defaults, poison-config protection. Very likely the actual design question for this team.
- **"How do you deploy safely?"** → Two layers: config rollout (instant, no deploy) and code rollout (readiness-gated, `maxUnavailable: 0`, stalls rather than breaks). Kill switch vs rollback.
- **"Tell me about a K8s outage."** → The liveness-probe-checks-the-database cascade. You'll have *seen* the stalled rollout in exercise 7.
- **"You're a frontend engineer — why should we hire you for backend?"** → Don't be defensive. You bring developer empathy (their stated value) and you demonstrably ship backend services; here's one, with tests, CI, and a design doc. Honest framing beats overclaiming, and this repo is the evidence.

## Where to go next

Only after the exercises: Postgres persistence → structured config propagation (etcd/Consul patterns) → OpenTelemetry tracing → the Google SRE book chapters on SLOs and release engineering → CNCF landscape basics (Prometheus, Envoy, Argo Rollouts — Argo especially, since it automates exactly what this repo does by hand).
