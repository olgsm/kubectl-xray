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

# capture a container's runtime env (works on distroless)
kubectl xray env <pod|deployment> -n <namespace> [-c <container>]
```

The debug container runs as the target's UID so it can read `/proc/1/environ`.
That UID is taken from the pod spec, or `--run-as-user`, or auto-discovered by a
quick probe when neither is set.

## Use cases

1. **Env from a distroless container** ✅ — read `/proc/<pid>/environ` from a
   UID-matched ephemeral toolbox container; no `env`/shell needed in the target.
2. **Capture dumps** _(planned)_ — JVM (jattach/async-profiler) and Go (dlv/pprof)
   under an admission-safe profile.
3. **Preserve + share sessions** _(planned)_ — capture termination context via a
   watch on the `Terminated` transition (before it's overwritten); save output +
   dumps; share a link; attach to an incident.

### TODO
- Redaction: env/dumps contain secrets; redact before anything leaves the cluster. Make it default-on. Allow to override.
- Remember used toolbox images, add a handy selector for it.
- Admission-safe profiles: restricted default + `--custom` overlay. Mimic kubectl debug with a handy default?
