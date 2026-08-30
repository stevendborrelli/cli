# Testing the combined PR branch

This branch exists only for testing. It merges four open PRs against
`crossplane/cli` so they can be exercised together. **Do not open a PR from
it** — each change is reviewable on its own.

It is rebuilt by hand whenever one of the PRs changes, so it may lag. Check the
merge commits against the PRs below before trusting a result.

## What is in it

| PR | Change |
|---|---|
| [#170](https://github.com/crossplane/cli/pull/170) | TypeScript support for projects: schema generation, function builder, templates |
| [#304](https://github.com/crossplane/cli/pull/304) | Cache function runtime base image layers between builds |

[#302](https://github.com/crossplane/cli/pull/302) (`--[no-]default-mrap`) and
[#303](https://github.com/crossplane/cli/pull/303) (error propagation) have
merged upstream, so they arrive through `main` rather than as merges here. They
are still worth exercising — see below — but a bug in either is a bug in `main`,
not in this branch.

Plus one commit that exists only here: #304 adds a parameter to
`baseImageForArch` and #170 adds a caller for it. The two merge cleanly because
they touch different files, but the result does not compile without wiring them
together. Whichever PR merges second upstream will need that one-line change.

## Build it

```shell
git clone -b integration-all-prs https://github.com/stevendborrelli/cli
cd cli
go build -o crossplane ./cmd/crossplane
./crossplane version --client
```

The version string carries the git hash, so quote it in any bug report.

## Try it

A TypeScript project end to end:

```shell
crossplane project init my-project && cd my-project
crossplane function generate mynetwork --language=typescript
crossplane project build
```

Then run it on a local dev control plane. `--init-resources` applies before the
Configuration is installed, `--extra-resources` after:

```shell
crossplane project run --init-resources=namespace.yaml \
                       --extra-resources=providerconfig.yaml \
                       --no-default-mrap
```

## What to look for

**#304, the base image cache.** The first build of a project is slow — it
fetches roughly 110MB of base image layers for a two-architecture build. Every
build after that should show `Writing packages to disk` completing in under a
second. If a second build still takes tens of seconds in that phase, that is a
bug worth reporting. The cache lives under `crossplane/base-images` in your
user cache directory; delete it to re-test cold. Nothing prunes it yet.

**#302, `--no-default-mrap`.** Verify it rather than assuming, because a
successful build says nothing about whether the flag worked:

```shell
kubectl get managedresourceactivationpolicy   # expect none
```

The flag only applies when the control plane is **created**. Reusing an
existing one silently keeps whatever it was built with, so run `crossplane
project stop` first or you will get a misleading result.

**#170, TypeScript.** A freshly generated function should pass `npm run build`,
`npm test`, and `npm run lint` with no edits. `docs/typescript-testing-guide.md`
walks through a full project, and its code samples are checked against the
generated models.

## Known rough edges

- Schema generation for the project's own XRDs runs on every build, cached or
  not. Expect several seconds per build even when nothing changed.
- `Building function <name>` is the slowest phase for TypeScript projects: it
  runs one npm install per target architecture plus the compile. Dropping
  `spec.architectures` to your native one while iterating helps.
- The base image cache has no size limit.
