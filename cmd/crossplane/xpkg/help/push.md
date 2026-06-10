The `xpkg push` command pushes a Crossplane package file to any OCI registry. A
package's OCI tag must be a semantic version. The `push` command uses registry
credentials from the local `docker` configuration; pushing to a private registry
may require a prior `docker login`.

By default the command looks in the current directory for a single `.xpkg` file
to push. To push multiple files (for example, a multi-platform package) or a
specific `.xpkg` file, use `-f` (`--package-files`).

## Destination tag

Specify the destination as a positional argument. The destination must be fully
qualified, including the registry, repository, and tag (for example,
`registry.example.com/package:v1.0.0`).

If no destination is provided, the command reads the tag embedded in the package
file. Use `xpkg build --tag` to embed a tag when building.

When pushing multiple package files without a destination argument, all packages
must have the same embedded tag.

## Examples

Push a package to its embedded tag destination:

```shell
crossplane xpkg build --tag=xpkg.crossplane.io/crossplane/my-config:v1.0.0
crossplane xpkg push -f my-config-*.xpkg
```

Push a multi-platform package:

```shell
crossplane xpkg push -f function-amd64.xpkg,function-arm64.xpkg \
  xpkg.crossplane.io/crossplane/function-example:v1.0.0
```

Push the single xpkg file in the current directory:

```shell
crossplane xpkg push xpkg.crossplane.io/crossplane/function-example:v1.0.0
```

Push to Docker Hub:

```shell
crossplane xpkg push docker.io/crossplane/function-example:v1.0.0
```
