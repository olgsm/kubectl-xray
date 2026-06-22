# kubectl-xray

> A kubectl plugin to inspect pods and capture execution evidence — even on distroless images — via ephemeral debug containers.

### Motivation

Sometimes you want to quickly grep pod's environment, you run `exec -- env | grep` as usual,
but at this point you might face the burden of **distroless containers**. The same goes for
collecting dumps and live profiling of a suspicious/failed pod, especially during the incident, 
when you don't have time to remember which profile is allowed to be attached via `kubectl debug`,
or which capabilities you have to drop.

Besides that, `kubectl debug` itself leaves no durable record: `EphemeralContainerStatus` has no
`lastState` field, so a session's termination context — exit code, duration, `--target` container,
debug logs — is lost the moment any pod update replaces `State.Terminated`. In some environments, 
this might even have a compliance impact: PCI-DSS 10.3 / SOC 2 require traceability, and 
questions like "who looked at what container, for how long" can't be answered from 
k8s audit logs alone ([source](https://www.cncf.io/blog/2026/05/18/what-kubectl-debug-doesnt-tell-you-the-silent-evidence-gap/)).

This tool aims to fill this gap — capture introspection output, dumps, and
session metadata locally. In plans are optional pushes to S3-like storage
with RBAC and shareable+expirable links.

## Usage

```sh
# build & install on PATH
go build -o kubectl-xray ./cmd/kubectl-xray && mv kubectl-xray /usr/local/bin/

# capture JVM dumps (thread + GC histogram + heap) into a local bundle
kubectl xray jvm-dump <pod|deployment> -n <namespace> [-c <container>] -o ./dumps

# env reads the target's /proc/1/environ (works on distroless)
kubectl xray env <pod|deployment> -n <namespace> [-c <container>]
```

Commands run in a **toolbox image** (`--image`) injected alongside the target,
sharing its PID namespace (reach the target's filesystem via `/proc/<pid>/root/`).
The debug container runs as the target's UID so it can read `/proc/1/...` and
attach to the JVM; that UID is derived from the pod spec, or set via `--run-as-user`, or is
auto-discovered by a quick probe when neither is set. `jvm-dump` writes artifacts
into `<output>/<pod>-<timestamp>/`; `env` streams to stdout (pipeable).

## Use cases

1. **Env from a distroless container** ✅ — read `/proc/<pid>/environ` from a
   UID-matched ephemeral toolbox container; no `env`/shell needed in the target.
2. **Capture dumps** ✅ — JVM (jattach/async-profiler) and Go (dlv/pprof) _(planned)_
   under an admission-safe profile.
3. **Preserve + share sessions** _(planned)_ — capture termination context via a
   watch on the `Terminated` transition (before it's overwritten); save output +
   dumps; share a link; attach to an incident.
