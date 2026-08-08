# Multi-stage build. Stage 1 compiles, stage 2 keeps only the binary.
# The result is a few MB instead of ~800MB, which matters in K8s: smaller images
# pull faster, so rollouts and scale-ups are faster, and there is less software
# in the image for a CVE scanner to find.

FROM golang:1.24-alpine AS build
WORKDIR /src

# Copy manifests first. Docker caches layers, so dependency downloads are only
# re-run when go.mod/go.sum change -- not on every source edit.
COPY go.mod ./
RUN go mod download

COPY . .

# CGO_ENABLED=0 produces a static binary with no libc dependency, which is what
# lets us run on `scratch` below.
# -ldflags "-s -w" strips debug symbols to shrink the binary further.
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /out/modelgate ./cmd/modelgate

# Run the tests inside the build so a broken image can never be produced.
RUN go vet ./... && go test ./...

# ---

FROM gcr.io/distroless/static-debian12:nonroot

# distroless has no shell, no package manager, no busybox. If someone gets RCE
# there is literally nothing to exec. Tradeoff: you cannot `kubectl exec` into
# it to poke around -- you debug via logs, metrics, and ephemeral debug
# containers instead. That is a real and defensible production tradeoff.

COPY --from=build /out/modelgate /modelgate

# Run as non-root. Many clusters enforce this with an admission policy and will
# refuse to schedule a pod that runs as UID 0.
USER nonroot:nonroot

EXPOSE 8080
ENTRYPOINT ["/modelgate"]
