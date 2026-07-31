# kubectl-xray

> A kubectl plugin to inspect pods and capture execution evidence via ephemeral debug containers. 
> Works on distroless out-of-the-box.

## Why

When an application pod misbehaves, the first two questions are how it is
configured and what its process is doing right now. Both are normally answered
by exec'ing into the container — the one thing a hardened image takes away.
Distroless and minimal bases ship no shell and no coreutils:

```sh
$ kubectl exec -it mypod -- env | grep DB_
OCI runtime exec failed: exec failed: unable to start container process:
exec: "env": executable file not found in $PATH
```

Diagnostics fail the same way, and not only on distroless — for example, a JRE-based image
carries no JDK tooling either, so there is nothing to take a thread or heap dump
with:

```sh
$ kubectl exec -it mypod -- jcmd 1 Thread.print
OCI runtime exec failed: exec failed: unable to start container process:
exec: "jcmd": executable file not found in $PATH
```

And a dump you do manage to take is written inside the container, so it still
has to be streamed out before the pod is replaced — otherwise the evidence goes
with it.

`kubectl debug` solves the general case, but each invocation requires getting
the details right — a security profile that passes admission, the capabilities
to drop, PID namespace sharing, and a UID that matches the target process so
`/proc` reads and JVM attach work. That is a lot to reconstruct while an
incident is open.

<details>
<summary>There is also an evidence gap in <code>kubectl debug</code></summary>

`kubectl debug` leaves no durable record: `EphemeralContainerStatus` has no `lastState` field, so a
session's termination context — exit code, duration, `--target` container, debug logs — is lost the
moment any pod update replaces `State.Terminated`. In some environments this has a compliance
impact: PCI-DSS 10.3 / SOC 2 require traceability, and questions like "who looked at what container,
for how long" can't be answered from k8s audit logs alone
([source](https://www.cncf.io/blog/2026/05/18/what-kubectl-debug-doesnt-tell-you-the-silent-evidence-gap/)).

</details>

xray infers the defaults it can and captures the evidence in one line:

```sh
$ kubectl xray env mypod --no-redact | grep DB_
$ kubectl xray jvm-dump mypod -o ./dumps
```

## Usage

```sh
# build & install on PATH (kubectl discovers kubectl-* binaries as subcommands)
make install   # → /usr/local/bin; override with INSTALL_DIR=~/bin

# capture JVM dumps (thread + GC histogram + heap) into a local bundle
kubectl xray jvm-dump <pod|deployment> -n <namespace> [-c <container>] -o ./dumps

# name steps to narrow it down (no step flags = thread + histogram)
kubectl xray jvm-dump <pod> --histogram

# a heap dump is opt-in: the hprof carries secrets and PII, unredactable
kubectl xray jvm-dump <pod> --heap

# virtual threads are invisible to jstack; JDK 21+ only, so it's opt-in
kubectl xray jvm-dump <pod> --vthreads

# JFR profiling (settings=profile), also opt-in — the capture waits it out
kubectl xray jvm-dump <pod> --jfr 60s

# capture Go pprof profiles (needs the app to serve net/http/pprof on --port)
kubectl xray go-dump <pod|deployment> -n <namespace> --port 6060 [--profile]

# env reads the target's /proc/1/environ (works on distroless)
kubectl xray env <pod|deployment> -n <namespace> [-c <container>]

# process, memory, cgroup limits and fd usage; optionally scrape app metrics
kubectl xray info <pod|deployment> -n <namespace> [--metrics-port 8080]

# open an interactive shell in a debug container beside the target
kubectl xray debug <pod|deployment> -n <namespace> [-c <container>] [--shell sh]
```

The target is a pod name, or kubectl's `TYPE/NAME` form (`pod/foo`, `deploy/foo`)
when a bare name would be ambiguous. Given a deployment, xray picks a ready pod
(newest among equals) and prints which one it chose.

Commands run in a **toolbox image** (`--image`) injected alongside the target,
sharing its PID namespace (reach the target's filesystem via `/proc/<pid>/root/`).
The debug container runs as the target's UID so it can read `/proc/1/...` and
attach to the JVM; that UID is derived from the pod spec, or set via `--run-as-user`, or is
auto-discovered by a quick probe when neither is set. `jvm-dump` writes artifacts
into `<output>/<pod>-<timestamp>/`; `env` streams to stdout (pipeable).

`jvm-dump` defaults to `eclipse-temurin:21-jdk`. The JDK tools attach dynamically
rather than parsing the target's memory, so they generally work against older
JVMs; if a step does fail on a version mismatch, point `--image` at a matching
JDK (`--image eclipse-temurin:17-jdk`).

## Use cases

1. **Env from a distroless container** ✅ — read `/proc/<pid>/environ` from a
   UID-matched ephemeral toolbox container; no `env`/shell needed in the target.
   Secret-looking values are masked heuristically by default so they never hit your terminal or logs (`--no-redact` to opt out).
2. **Capture dumps** ✅ — JVM (jstack, GC histogram, jmap heap, JDK 21 virtual
   threads, JFR) and Go (goroutine, heap, CPU pprof), streamed into one verified
   bundle under an admission-safe profile.
3. **Runtime facts** ✅ — `info` reports the target's command line, threads,
   RSS/PSS, cgroup limits and fd usage from `/proc`, and optionally scrapes the
   app's metrics endpoint over the pod's network namespace.
4. **Interactive debug shell** ✅ — drop into a UID-matched toolbox container
   sharing the target's PID namespace, no need to recall the image/caps/profile.
5. **Preserve + share sessions** _(planned)_ — capture termination context, save output +
   dumps to S3 storage; share a link; attach to an incident.
6. **Smart toolbox image** _(planned)_ — pick the image from the tools you ask for
   (`--tools jstack,tcpdump` → `some internal kubectl-toolkit`/`netshoot`/…) instead of always
   defaulting to busybox; allow adding/choosing quickly; infer and honor the cluster's admission constraints.
