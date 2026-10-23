# kubectl-xray

[![CI](https://github.com/olgsm/kubectl-xray/actions/workflows/ci.yml/badge.svg)](https://github.com/olgsm/kubectl-xray/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/olgsm/kubectl-xray)](https://github.com/olgsm/kubectl-xray/releases)
[![License](https://img.shields.io/github/license/olgsm/kubectl-xray)](LICENSE)

**Inspect any pod and capture JVM/Go dumps in one command — distroless included, secrets redacted.**

`kubectl xray` helps to run tools in an ephemeral container beside your app. It works out
the pod, UID, namespaces and security profile for you, and allows to retrieve environment variable, runtime facts
or dumps straight to your terminal or a local file.

```console
$ kubectl xray env deploy/checkout -n shop | grep DB_
xray: found 3 pods for deployment checkout, using pod/checkout-7d9f8c-x2k4q
DB_HOST=postgres.shop.svc
DB_PASSWORD=***REDACTED***
DB_URL=***REDACTED***
xray: masked 2 secret-looking value(s); use --no-redact to disable

$ kubectl xray jvm-dump deploy/checkout -n shop --thread --heap
xray: found 3 pods for deployment checkout, using pod/checkout-7d9f8c-x2k4q
xray: injecting jvm-dump toolbox xray-debug-3f9a1c (image eclipse-temurin:21-jdk, UID 1000) in shop/checkout-7d9f8c-x2k4q...
xray: capturing dumps...
xray: wrote dumps/checkout-7d9f8c-x2k4q-2026-10-22_14-03-11.tar.gz (2 artifacts, toolbox exited 0)
```

## Features

- **Works on any image.** Distroless, scratch or JRE-only — the tools live in the
  toolbox container.
- **Infers the fiddly parts.** Point it at a pod or `deploy/NAME`: xray picks a ready
  pod and the default container, matches the app's UID (from the pod spec, or by
  probing the running process), shares its PID namespace and runs under a security
  profile that passes Pod Security `restricted`.
- **Dumps land on your machine.** Artifacts are tarred, gzipped and streamed out of
  the pod into a single `dumps/<pod>-<timestamp>.tar.gz` — no `kubectl cp`, and the
  app's `/tmp` is cleaned up after. Each bundle is verified (toolbox exit code,
  artifact count, gzip CRC) and written atomically with `0600` permissions.
- **Many kinds of dumps.** JVM: thread dump, GC class histogram, heap dump, virtual
  threads (JDK 21+), JFR recording. Go: goroutine dump, heap and CPU profiles via
  `net/http/pprof`.
- **Secrets are redacted by default.** Secret-looking env values are masked before they reach your
  terminal — by name (`*PASSWORD*`, `*TOKEN*`, `*API_KEY*`, …) or by shape (JWTs, PEM
  keys, `user:pass@` URLs). Heap dumps have every primitive array zeroed (`String`,
  `byte[]`, `char[]` contents) while keeping the object graph and sizes for leak
  analysis, so a raw hprof never touches your disk. JFR recordings are stripped of env
  vars, system properties, the command line and exception messages. `--no-redact`
  opts out.
- **Runtime facts and a shell.** `info` shows the command line, threads, RSS/PSS,
  cgroup limits and fd usage, plus app metrics with `--metrics-port`; `debug` opens an
  interactive shell beside the app.

## Why

Distroless and JRE-only images ship without a shell or JDK tools, so `kubectl exec`
has nothing to run:

```console
$ kubectl exec mypod -- env
exec: "env": executable file not found in $PATH
$ kubectl exec mypod -- jcmd 1 Thread.print
exec: "jcmd": executable file not found in $PATH
```

`kubectl debug` (or [cdebug](https://github.com/iximiuz/cdebug)) can attach a toolbox,
but only once you get the details right: a security profile your cluster admits, a
shared PID namespace, and the app's UID (without it, `/proc` reads and JVM attach
fail). Any dump you take still has to be copied out before the pod is gone. xray does
all of it in one command.

## Installation

With [krew](https://krew.sigs.k8s.io/docs/user-guide/setup/install/):

```sh
kubectl krew index add olgsm https://github.com/olgsm/kubectl-xray.git
kubectl krew install olgsm/xray
```

Upgrade later with `kubectl krew upgrade`.

Or grab an archive for your OS from [Releases](https://github.com/olgsm/kubectl-xray/releases)
and put `kubectl-xray` on your `PATH`: kubectl runs any `kubectl-*` binary there as
a subcommand. With Go installed:

```sh
go install github.com/olgsm/kubectl-xray/cmd/kubectl-xray@latest
```

This installs into `$(go env GOPATH)/bin`, which must be on your `PATH`. From a
checkout, `make install` copies the binary to `/usr/local/bin` (override with
`INSTALL_DIR=~/bin`).

**Requirements:** Kubernetes 1.25+ (ephemeral containers) and permission to update
`pods/ephemeralcontainers` and attach to pods in the target namespace.

<details>
<summary>Minimal Role</summary>

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: kubectl-xray
rules:
  - apiGroups: [ "" ]
    resources: [ pods ]
    verbs: [ get, list, watch ]
  - apiGroups: [ "" ]
    resources: [ pods/ephemeralcontainers ]
    verbs: [ update ]
  - apiGroups: [ "" ]
    resources: [ pods/attach ]
    verbs: [ create ]
  - apiGroups: [ "" ]
    resources: [ pods/log ]
    verbs: [ get ]
  - apiGroups: [ apps ]
    resources: [ deployments ] # only for deploy/NAME targets
    verbs: [ get ]
```

</details>

## Usage

```
kubectl xray <command> <pod|pod/NAME|deploy/NAME> [-n NAMESPACE] [-c CONTAINER] [flags]
```

A bare name is tried as a pod, then as a deployment. Given a deployment, xray picks a
ready pod (newest first) and tells you which.

| Command        | Captures                                                                                         |
|----------------|--------------------------------------------------------------------------------------------------|
| `env`          | the app's environment (`/proc/1/environ`), secrets masked                                        |
| `info`         | command line, threads, RSS/PSS, cgroup limits, open fds; app metrics with `--metrics-port`       |
| `jvm-dump`     | thread dump and GC histogram; opt in to `--heap`, `--vthreads`, `--jfr 60s`                      |
| `go-dump`      | goroutine dump and heap profile; opt in to `--profile` (10s CPU). Needs pprof on `--port` (6060) |
| `debug` (`sh`) | an interactive shell in the toolbox, beside the app                                              |

Naming any dump step captures only what you name: `jvm-dump --heap` takes just the heap.

```sh
kubectl xray env deploy/api | grep DB_
kubectl xray info deploy/api --metrics-port 8080
kubectl xray jvm-dump deploy/api --heap
kubectl xray jvm-dump deploy/api --jfr 60s --vthreads   # holds the capture open for 60s
kubectl xray go-dump deploy/api --profile
kubectl xray debug deploy/api
```

| Flag             |                                                                             |
|------------------|-----------------------------------------------------------------------------|
| `--image`        | toolbox image; `busybox:1.36`, or `eclipse-temurin:21-jdk` for `jvm-dump`   |
| `--run-as-user`  | toolbox UID, instead of the inferred one                                    |
| `-o`, `--output` | directory for dump bundles (default `dumps`)                                |
| `--extract`      | unpack the bundle into a directory instead of a `.tar.gz`                   |
| `--max-size`     | fail rather than write a bundle larger than this, e.g. `2Gi`                |
| `--dump-dir`     | leave artifacts in a directory inside the pod instead of streaming them out |
| `--no-redact`    | keep env values, heap dumps and JFR raw                                     |

`--dump-dir` is for an uploader that watches a volume: the directory must already be
mounted in the pod and writable by the app's UID. The JVM writes heap, virtual-thread
and JFR files straight there, so a multi-GB heap is never copied twice — but nothing
is redacted.

The JDK tools attach dynamically rather than parse the target's memory, so the JDK 21
toolbox generally works against older JVMs. If a step fails on a version mismatch, use
a matching JDK: `--image eclipse-temurin:17-jdk`.

## How it works

Each command injects an [ephemeral container](https://kubernetes.io/docs/concepts/workloads/pods/ephemeral-containers/)
— the toolbox — that shares the app's PID namespace (its filesystem is under
`/proc/1/root/`) and runs as the app's UID. Matching the UID is what lets it read
`/proc/1/environ` and attach to the JVM without extra capabilities; the UID comes from
the pod spec, `--run-as-user`, or a quick probe of `/proc/1/status` when neither sets
it. The toolbox runs non-root with all capabilities dropped, no privilege escalation
and the `RuntimeDefault` seccomp profile.

`env` and `info` write to stdout while progress goes to stderr, so output stays
pipeable. Dump commands tar and gzip their artifacts in the toolbox and stream them
out over the attach connection; the capture fails if the toolbox exits non-zero or an
artifact is missing, so a partial bundle never passes as complete.

Ephemeral containers can't be removed from a pod: each run leaves a terminated
`xray-debug-*` container behind until the pod is replaced.

## Roadmap

- **Session records** — keep each session's termination context, output and dumps (e.g. in S3), shareable as a link and
  attachable to an incident. `kubectl debug`
  leaves no durable record of who looked at which container and for how long — a gap
  for PCI-DSS 10.3 / SOC 2
  ([details](https://www.cncf.io/blog/2026/05/18/what-kubectl-debug-doesnt-tell-you-the-silent-evidence-gap/)).

## Contributing

Issues and PRs are welcome. Run `make test` and `make lint` before sending one; commit
messages follow [Conventional Commits](https://www.conventionalcommits.org), which
drive releases.

## License

[Apache 2.0](LICENSE)
