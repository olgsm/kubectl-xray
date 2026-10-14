# Changelog

## 1.0.0 (2026-10-14)


### Features

* add --version from build VCS info ([8cf3dc0](https://github.com/olgsm/kubectl-xray/commit/8cf3dc0eeecb94f8071b85c9e29f6d39bde26ba9))
* **debug:** add interactive debug TTY mode ([ffd480a](https://github.com/olgsm/kubectl-xray/commit/ffd480a8b33b40c9e5da724a333803d7a1f5e4c7))
* **dump:** add --dump-dir to leave artifacts in a volume inside the target; update README ([302fc18](https://github.com/olgsm/kubectl-xray/commit/302fc18ab48f78cc4582fc4dbf61bb077efe83e1))
* **env:** capture env from distroless pods via ephemeral debug containers ([c5acf8b](https://github.com/olgsm/kubectl-xray/commit/c5acf8b80c77b0f28f86a87445b8b7b0299f0e13))
* **env:** redact secret-looking values by default (--no-redact to disable) ([45af1a8](https://github.com/olgsm/kubectl-xray/commit/45af1a8440ca5f9e44d5b9bffc361235b11f5685))
* **go-dump:** capture Go pprof profiles via shared net namespace (PoC) ([74db001](https://github.com/olgsm/kubectl-xray/commit/74db0015309f0acdf10a62d5082ec8bb1fcc7be0))
* **info:** report process, memory, cgroup limits and optional app metrics ([19e6e91](https://github.com/olgsm/kubectl-xray/commit/19e6e919089232188a3c312f38a4bc4f4373bbbe))
* **jvm-dump:** add --max-size guard against filling local disk ([3c412d1](https://github.com/olgsm/kubectl-xray/commit/3c412d1fe84b8962358ae5b8c1c71178bdc490dc))
* **jvm-dump:** add --vthreads and --jfr steps, make --heap opt-in ([e0b2755](https://github.com/olgsm/kubectl-xray/commit/e0b2755c8db908940c3817c1deffc976ff3d6f57))
* **jvm-dump:** add basic JVM dumps collection command ([6d7f76d](https://github.com/olgsm/kubectl-xray/commit/6d7f76d2c39a44a9c0d1b27fe042dadc0f6e82be))
* **jvm-dump:** make .tar.gz unpack optional via --extract ([f5982c7](https://github.com/olgsm/kubectl-xray/commit/f5982c7e01a72f7af07782d2913bea33b220eeb0))
* **jvm-dump:** redact heap dumps and JFR recordings by default ([3b0e86d](https://github.com/olgsm/kubectl-xray/commit/3b0e86d5b143bad668c1c7c2215cca50b32769d6))
* **makefile:** add simple Makefile ([8059149](https://github.com/olgsm/kubectl-xray/commit/80591498260b3b5048b2ef080563112285b8c512))


### Bug Fixes

* **dump:** fail on incomplete captures; keep the attach stream warm ([f1db0be](https://github.com/olgsm/kubectl-xray/commit/f1db0be5a87ef8e3dc5804f91bc3b7642288f976))
* **dump:** make step flags additive instead of default-on ([9a95194](https://github.com/olgsm/kubectl-xray/commit/9a951946358df3d079125d95771e46257ac03c24))
* **ephemeral:** set StdinOnce so closing the attach lets the toolbox exit ([95ec707](https://github.com/olgsm/kubectl-xray/commit/95ec707491bbef02b88e8a84b38c45097c4206a5))
* **jvm-dump:** remove heap dump from target /tmp after copying it out ([b5d630a](https://github.com/olgsm/kubectl-xray/commit/b5d630ab46c9b6f811d3a7d0d743bc8a51f425b5))
* **jvm-dump:** stop dropping tool stderr so dump failures surface ([c99d25b](https://github.com/olgsm/kubectl-xray/commit/c99d25b69055b25ceeff2d052d11731460611d52))
* **target:** accept TYPE/NAME and pick a ready pod, reporting which ([5407d10](https://github.com/olgsm/kubectl-xray/commit/5407d10dfdd6ba76be12fcccfa4fd1efd404cb86))
