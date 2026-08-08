# modelgate

A small Go service that decides **which AI model a given client should use**, and lets operators roll a model out gradually or kill it instantly.

Built as a hands-on learning project for Go, HTTP API design, progressive delivery, and Kubernetes. It deliberately mirrors the problem domain of a model-lifecycle/rollout team: getting new model configurations out to a very large client fleet *safely*.

**New here? Read [docs/LEARNING.md](docs/LEARNING.md)** — it is the full teaching guide with the weekend plan, concept explanations, and exercises.

---

## Problem

You have a fleet of clients and several candidate models. You want to move traffic onto a new model gradually — 1%, then 5%, then 25%, then everyone — and you want to undo that instantly if it misbehaves. Requirements:

1. **Sticky.** A client must get the same answer every time. Flapping between models mid-session is a bad experience and makes incidents impossible to reason about.
2. **Monotonic.** Raising a rollout percentage may only *add* clients. Nobody already on the new model gets moved back.
3. **Fast and dependency-free on the read path.** An assignment lookup can't require a database round trip.
4. **Instantly reversible.** A kill switch that takes effect on the next request.
5. **Consistent across replicas.** Three pods must give the same client the same answer without talking to each other.

## API

| Method | Path | Purpose |
| --- | --- | --- |
| `GET` | `/v1/assignment?client_id=abc` | Which model should this client use |
| `GET` | `/v1/models` | List models and rollout state |
| `PUT` | `/v1/models/{id}/rollout` | Set rollout percentage — `{"percent": 25}` |
| `POST` | `/v1/models/{id}/disable` | Kill switch |
| `POST` | `/v1/models/{id}/enable` | Undo the kill switch |
| `GET` | `/livez` | Liveness — is the process wedged |
| `GET` | `/readyz` | Readiness — should this pod get traffic |
| `GET` | `/metrics` | Prometheus exposition |

```json
GET /v1/assignment?client_id=user-2
{"model":"gpt-5-mini","reason":"rollout:25%"}
```

## The core design decision: deterministic hash bucketing

```go
bucket := fnv1a(modelID + "\x00" + clientID) % 100
enrolled := bucket < percent
```

This one line satisfies requirements 1, 2, 3, and 5 at once:

- **Sticky and stateless** — the same input always produces the same bucket, so no storage and no coordination are needed. Any replica computes the same answer independently.
- **Monotonic** — enrollment is `bucket < percent`. Raising `percent` can only let *more* buckets through. It is arithmetically impossible to demote an enrolled client. There is a test that ramps 0→100 in steps of 5 across 5,000 clients and fails if anyone is ever demoted.
- **Salted per model** — hashing the model ID in first means the same unlucky clients aren't the guinea pigs for every single rollout. Without the salt, `client-7` would be in the first 1% of every experiment forever.

## Failure modes and the choices made

| Situation | Behaviour | Why |
| --- | --- | --- |
| No models configured | Serve `baseline` | **Fail open.** Returning an error would break every client. A degraded-but-working answer beats an outage. |
| Store not yet loaded at boot | `/readyz` returns 503 | Pod is alive but must not receive traffic until it can answer correctly. |
| Store unavailable (future, with a DB) | Keep serving last-known config | The read path must survive a control-plane outage. |
| Model misbehaving in production | `POST /disable` — next request falls back | Kill switch is one call and needs no deploy. |
| Bad deploy | Rollout stalls, old pods keep serving | `maxUnavailable: 0` + readiness gating. See below. |

## Rollout and rollback

```bash
# Ramp
curl -X PUT localhost:8080/v1/models/gpt-5-preview/rollout -d '{"percent":1}'
curl -X PUT localhost:8080/v1/models/gpt-5-preview/rollout -d '{"percent":25}'

# Panic
curl -X POST localhost:8080/v1/models/gpt-5-preview/disable

# Service-level rollback
kubectl rollout undo deployment/modelgate
```

Two independent layers of safety: **config rollout** (percentage + kill switch, instant, no deploy) and **service rollout** (K8s rolling update, `maxUnavailable: 0`, gated on readiness so a broken build stalls rather than breaks).

## Run it

```bash
go test ./...                                    # all tests
go run ./cmd/modelgate                           # :8080
STARTUP_DELAY_SECONDS=10 go run ./cmd/modelgate  # watch /readyz go 503 -> 200

docker build -t modelgate:dev .                  # 14MB distroless image

kind create cluster --name modelgate
kind load docker-image modelgate:dev --name modelgate
kubectl apply -f deploy/k8s/deployment.yaml
kubectl rollout status deployment/modelgate
```

## Verified behaviour

Everything below was actually run, not assumed:

- All tests pass, `go vet` clean, zero third-party dependencies.
- `/readyz` returns 503 → 200 at the seed boundary while `/livez` stays 200 throughout.
- Under continuous load, SIGTERM drained **69 requests with zero 5xx** before the listener closed.
- Container image is 14.1MB, runs as `nonroot`, tests execute inside the Docker build.

## What I'd add for production

- **Persistence** (Postgres) behind the existing `store.Store` interface — the handlers would not change.
- **Audit log** on every rollout change: who, when, old → new. Non-negotiable during an incident.
- **Latency histogram**, not just counters — you cannot compute a p99 from a counter, and the SLO is a p99.
- **Automated canary analysis** — watch error rate after each ramp step and auto-halt.
- **Auth** on the write endpoints, which are currently unauthenticated by design for local learning.
- **Multi-region caching** with an explicit propagation-delay budget.

## Layout

```
cmd/modelgate/      entrypoint, graceful shutdown
internal/rollout/   assignment logic + the interesting tests
internal/store/     Store interface + in-memory impl
internal/httpapi/   routes, handlers, middleware, metrics
deploy/k8s/         handwritten Deployment, Service, PDB
docs/LEARNING.md    the teaching guide
```

There are numbered `TODO(exercise N)` markers through the code — see [docs/LEARNING.md](docs/LEARNING.md).
