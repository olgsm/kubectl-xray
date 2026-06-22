# kubectl-xray — working notes for Claude

A kubectl plugin (Go 1.26) that inspects pods and captures execution dumps via
ephemeral debug containers. Built incrementally.

## Code conventions

- **No narration comments in committed code.** Don't annotate one-line or self-evident
  fixes. Keep only comments that earn their place: proper Godoc on declarations, and
  short notes explaining a non-obvious *why* (e.g. why GET vs POST, why a
  redirection instead of a pipe). Strip explanatory chatter before committing.
