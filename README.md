# kubectl-xray

> A kubectl plugin to inspect pods and capture execution evidence via ephemeral debug containers. 
> Works on distroless out-of-the-box.

## Why

When a pod misbehaves, you generally want two things: its config, and what the process is
doing right now. Normally you just exec into the container. On a hardened image
you can't — there is no shell and no tools to run:

```sh
$ kubectl exec -it mypod -- env | grep DB_
exec: "env": executable file not found in $PATH

$ kubectl exec -it mypod -- jcmd 1 Thread.print
exec: "jcmd": executable file not found in $PATH
```

This is not only about distroless — a JRE image has no JDK tools either. And if
you do get a dump, it sits inside the container: you have to copy it out before
the pod is replaced, or it's gone.

`kubectl debug` (and `cdebug` too) can help, but you have to get every detail right: a
security profile your cluster accepts, which capabilities to drop, sharing the
PID namespace, and the right UID — otherwise `/proc` reads and JVM attach fail.

<details>
<summary>There is also an evidence gap in <code>kubectl debug</code></summary>

`kubectl debug` leaves no durable record: `EphemeralContainerStatus` has no `lastState` field, so a
session's termination context — exit code, duration, `--target` container, debug logs — is lost the
moment any pod update replaces `State.Terminated`. In some environments this has a compliance
impact: PCI-DSS 10.3 / SOC 2 require traceability, and questions like "who looked at what container,
for how long" can't be answered from k8s audit logs alone
([source](https://www.cncf.io/blog/2026/05/18/what-kubectl-debug-doesnt-tell-you-the-silent-evidence-gap/)).

</details>

`xray` infers the default values itself and captures the evidence in one line:

```sh
$ kubectl xray env mypod --no-redact | grep DB_    # find the DB config
$ kubectl xray jvm-dump --heap mypod -o ./dumps    # heap dump into a local .tar.gz
```

## Usage

```sh
# build & install on PATH (kubectl discovers kubectl-* binaries as subcommands)
make install   # → /usr/local/bin; override with INSTALL_DIR=~/bin

# capture JVM heap dump into a local bundle OR into a volume mounted in the pod
# (must be writable by the app's UID); note: a heap dump carries secrets and PII!
kubectl xray jvm-dump <pod> -n <namespace> [-c <container>] --heap -o ./dumps
kubectl xray jvm-dump <pod> -n <namespace> [-c <container>] --heap --dump-dir /dumps

# name dump steps to opt-in (no flags = only threads + histogram)
kubectl xray jvm-dump <pod> --vthreads   # JDK 21+
kubectl xray jvm-dump <pod> --jfr 60s    # holds the capture open 60s

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
auto-discovered by a quick probe when neither is set. `jvm-dump` and `go-dump`
write `<output>/<pod>-<timestamp>.tar.gz` (a directory instead with `--extract`,
or nothing locally with `--dump-dir`); `env` and `info` stream to stdout
(pipeable).

`--dump-dir` leaves the artifacts in a directory inside the target instead of
streaming them back — for an uploader that watches it. The directory has to be
mounted in the pod already and writable by the app's UID; the JVM writes its own
files (heap, virtual threads, JFR) straight there, so a multi-GB heap is never
copied twice.

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
