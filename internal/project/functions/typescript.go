/*
Copyright 2026 The Crossplane Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package functions

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"path"
	"path/filepath"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/spf13/afero"
	"golang.org/x/sync/errgroup"

	"github.com/crossplane/crossplane-runtime/v2/pkg/errors"
	"github.com/crossplane/crossplane-runtime/v2/pkg/xpkg"

	pkgv1beta1 "github.com/crossplane/crossplane/apis/v2/pkg/v1beta1"

	"github.com/crossplane/cli/v2/internal/docker"
	"github.com/crossplane/cli/v2/internal/filesystem"
	clixpkg "github.com/crossplane/cli/v2/internal/xpkg"
)

const (
	// typescriptBuildImage is the image in which we build the function.
	typescriptBuildImage = "docker.io/library/node:24.20.0-slim"
	// typescriptCompilerVersion is the TypeScript version used when a function
	// declares none of its own. A function that does declare one is compiled
	// with that instead; see typescriptBuildScript. Keep it in step with the
	// version the function template scaffolds.
	typescriptCompilerVersion = "7.0.2"
	// sharedFunctionPath is where the build script stages the function tree when
	// it is the same for every architecture. Its basename becomes the image's
	// working directory, so it differs from the per-architecture layout — see
	// configureTypescriptImage.
	sharedFunctionPath = "/fn_shared"
	// typescriptRuntimeImage is the distroless base used at runtime. The
	// :nonroot variant, so the built function does not serve gRPC as root.
	typescriptRuntimeImage = "gcr.io/distroless/nodejs24-debian13:nonroot"
	// typescriptBuildScript is the shell pipeline that runs in the build
	// container.
	//
	// SCHEMAS_PATH is the absolute path to the generated TypeScript schemas, or
	// empty if the project has none. ARCHS is the space-separated list of
	// target architectures, in npm's naming (see npmArchitecture).
	typescriptBuildScript = `set -eu
# Put the TypeScript compiler on PATH before installing anything else.
#
# TypeScript 7 ships as a per-platform package holding a statically linked
# binary plus the bundled lib declarations, and nothing else, so it installs in
# about a second and pulls no dependency tree. Installing it separately is what
# lets the function tree below be installed --omit=dev: the build used to need a
# project's devDependencies present purely so that tsc existed, and then
# reinstalled without them to get a tree fit to ship.
#
# The binary resolves its bundled lib.d.ts relative to its own location and
# panics if moved, so it stays where npm put it and a wrapper goes on PATH.
# A wrapper rather than a direct call means the project's own "build" script
# still runs, whatever that script does.
#
# The version comes from the project, so a function is compiled with the
# compiler it asks for rather than one the CLI chose. TypeScript 7 is normally
# declared as an npm alias, npm:typescript@<range>, because the plain
# "typescript" name is taken by the 6.x compiler that typescript-eslint needs,
# so the alias is what to look for. A plain "typescript" entry that is not an
# alias is used as-is. Failing both, the CLI's own pinned version applies.
#
# The per-platform packages are versioned in lockstep with typescript itself, so
# whatever range the project declares resolves the same way here.
TSC_SPEC=$(node -e '
const p = require("./package.json");
const deps = { ...(p.dependencies || {}), ...(p.devDependencies || {}) };
let spec = "";
for (const v of Object.values(deps)) {
  const m = /^npm:typescript@(.+)$/.exec(String(v));
  if (m) { spec = m[1]; break; }
}
if (!spec && deps.typescript && !String(deps.typescript).startsWith("npm:")) {
  spec = String(deps.typescript);
}
process.stdout.write(spec);
')
if [ -z "$TSC_SPEC" ]; then
  TSC_SPEC=$TSC_VERSION
  echo "no typescript dependency found; compiling with $TSC_SPEC"
fi
mkdir -p /tsc
npm install --no-save --no-fund --prefix /tsc "@typescript/typescript-linux-$(node -p 'process.arch')@$TSC_SPEC"
TSC_BIN=$(find /tsc -path '*/lib/tsc' -type f | head -1)
printf '#!/bin/sh\nexec %s "$@"\n' "$TSC_BIN" > /usr/local/bin/tsc
chmod +x /usr/local/bin/tsc

# Install dependencies for the schemas package so TypeScript can resolve the
# base types.
if [ -n "$SCHEMAS_PATH" ] && [ -d "$SCHEMAS_PATH" ] && [ -f "$SCHEMAS_PATH/package.json" ]; then
    cd "$SCHEMAS_PATH" && npm install --no-fund
    cd -
fi

# One install, serving both the compile and the image. --omit=dev keeps
# build-only packages out of both, and out of the manifest that ships.
npm install --omit=dev --no-fund
npm run build
node -e 'const f="package.json",p=require("./"+f);delete p.devDependencies;require("fs").writeFileSync(f,JSON.stringify(p,null,2)+"\n")'

# Decide whether the tree has to be built once per architecture at all.
#
# The per-architecture installs exist so that packages shipping per-platform
# binaries resolve for the target rather than for the build host. A tree where
# no such package ships has nothing to resolve differently, and installing it
# twice produces byte-identical output. The lockfile records cpu and os, so it
# answers the question without walking thousands of files.
#
# Only packages that reach the runtime tree count. Build-only packages are
# excluded because they are not installed here at all — and they are the common
# case: a project using vitest pulls in 47 per-platform @rolldown/binding
# entries, every one of them dev-only. Counting those would force
# per-architecture installs for almost every project while changing nothing
# about what ships. devOptional entries are reachable either way, so they count.
#
# A project with no lockfile is treated as needing per-architecture installs.
NEEDS_PER_ARCH=yes
if [ -f package-lock.json ]; then
  NEEDS_PER_ARCH=$(node -e 'const l=require("./package-lock.json");process.stdout.write(Object.values(l.packages||{}).some(p=>(p.cpu||p.os)&&!p.dev)?"yes":"no")')
fi

# When the tree is architecture-independent, stage it once. Copying it per
# architecture would make the CLI stream an identical tree out of the container
# for each one, which is the largest remaining cost of a build.
if [ "$NEEDS_PER_ARCH" = no ]; then
  set -- shared
else
  set -- $ARCHS
fi

for arch in "$@" ; do
  if [ "$NEEDS_PER_ARCH" = yes ]; then
    rm -rf node_modules
    # --cpu/--os select the right prebuilt artifacts for packages that publish
    # one per platform. They only steer optional-dependency selection: they do
    # not cross-compile node-gyp source builds, so a dependency that compiles
    # from source still produces build-host output.
    npm install --omit=dev --no-fund --cpu=$arch --os=linux
  fi
  mkdir -p /fn_$arch
  # Ship only what the function needs in order to run: the compiled output, the
  # runtime dependencies, and a package.json — Node needs its "type": "module"
  # to load dist/ as ESM.
  #
  # -L dereferences symlinks so file: dependencies (like crossplane-models) are
  # copied as real files rather than links that will not resolve at runtime.
  cp -rL node_modules /fn_$arch/
  cp -r dist /fn_$arch/
  # Drop file: dependencies from the manifest that ships. Those packages are
  # vendored into node_modules above, but the paths they came from do not exist
  # inside the image, so declaring them would make any npm install or npm ci run
  # there fail on a package it cannot resolve.
  node -e 'const p=require("./package.json");const d=p.dependencies||{};for(const k of Object.keys(d))if(String(d[k]).startsWith("file:"))delete d[k];process.stdout.write(JSON.stringify(p,null,2)+"\n")' > /fn_$arch/package.json
done
`
)

// typescriptBuilder builds TypeScript composition functions.
//
// A TypeScript embedded function is a full function-sdk-typescript project
// (package.json + tsconfig.json). We build it by running npm install and npm run build
// (which invokes tsc) in a Node.js build container, then copy the dist/
// and node_modules/ onto a distroless Node.js base. Runtime dependencies are
// installed once per target architecture, so each image gets a node_modules
// matching the architecture it will run on.
type typescriptBuilder struct {
	buildImage   string
	runtimeImage string
	transport    http.RoundTripper
	configStore  xpkg.ConfigStore
}

func (b *typescriptBuilder) Name() string {
	return "typescript"
}

func (b *typescriptBuilder) match(fromFS afero.Fs) (bool, error) {
	hasPackageJSON, err := afero.Exists(fromFS, "package.json")
	if err != nil {
		return false, err
	}
	hasTSConfig, err := afero.Exists(fromFS, "tsconfig.json")
	if err != nil {
		return false, err
	}
	return hasPackageJSON && hasTSConfig, nil
}

func (b *typescriptBuilder) Build(ctx context.Context, c BuildContext) ([]v1.Image, error) {
	if err := docker.Check(ctx); err != nil {
		return nil, errors.Wrap(err, "cannot build the TypeScript function because Docker is unavailable; start or install Docker, then retry")
	}

	functionTars, sharedWorkDir, err := b.buildFunction(ctx, c)
	if err != nil {
		return nil, err
	}

	runtimeImage := b.runtimeImage
	_, rewritten, err := b.configStore.RewritePath(ctx, b.runtimeImage)
	if err != nil {
		return nil, errors.Wrap(err, "failed to rewrite runtime image")
	}
	if rewritten != "" {
		runtimeImage = rewritten
	}

	runtimeRef, err := name.ParseReference(runtimeImage)
	if err != nil {
		return nil, errors.Wrap(err, "failed to parse typescript runtime base image")
	}

	images := make([]v1.Image, len(c.Architectures))
	eg, _ := errgroup.WithContext(ctx)
	for i, arch := range c.Architectures {
		eg.Go(func() error {
			baseImg, err := baseImageForArch(runtimeRef, arch, b.transport, c.BaseImageCacheDir)
			if err != nil {
				return errors.Wrap(err, "failed to fetch typescript runtime base image")
			}

			functionLayer, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) {
				return io.NopCloser(bytes.NewReader(functionTars[arch])), nil
			})
			if err != nil {
				return errors.Wrap(err, "failed to create function layer")
			}

			img, err := mutate.AppendLayers(baseImg, functionLayer)
			if err != nil {
				return errors.Wrap(err, "failed to append function layer")
			}

			img, err = configureTypescriptImage(img, arch, sharedWorkDir)
			if err != nil {
				return errors.Wrap(err, "failed to configure typescript image")
			}

			images[i] = img
			return nil
		})
	}

	return images, eg.Wait()
}

// buildFunction runs the build container against the function source and
// returns tars of /fn_<arch> for each architecture, suitable for use as image
// layers.
//
// The function source is staged at /<FunctionPath> in the build container and, if a
// typescript schemas tree exists, /<SchemasPath>/typescript/models/ — preserving
// the project's relative layout so that npm resolves the schemas path-dep from
// package.json. The compile step runs once, but the runtime dependencies are
// installed once per target architecture so that packages shipping per-platform
// binaries resolve correctly; see typescriptBuildScript.
//
//nolint:contextcheck // The defer uses context.Background() intentionally for cleanup.
func (b *typescriptBuilder) buildFunction(ctx context.Context, c BuildContext) (map[string][]byte, string, error) {
	fnFS := c.FunctionFS()
	// Exclude node_modules the user might have created locally.
	// Use the function path as the tar prefix so files end up at /<FunctionPath> in the container.
	fnTar, err := filesystem.FSToTar(fnFS, c.FunctionPath, filesystem.WithExcludePrefix("node_modules"))
	if err != nil {
		return nil, "", errors.Wrap(err, "failed to tar function source")
	}

	// Check if TypeScript schemas exist and tar them if so.
	// The schemas are placed at /<SchemasPath>/typescript/ to match
	// the relative path in package.json (e.g., "file:../../schemas/typescript").
	tsSchemasRel := path.Join(c.SchemasPath, "typescript")
	tsSchemasFS := afero.NewBasePathFs(c.ProjectFS, tsSchemasRel)
	hasTSSchemas, err := afero.DirExists(tsSchemasFS, ".")
	if err != nil {
		return nil, "", errors.Wrapf(err, "cannot check for TypeScript schemas at %q", tsSchemasRel)
	}
	var schemasTar []byte
	if hasTSSchemas {
		schemasTar, err = filesystem.FSToTar(tsSchemasFS, tsSchemasRel)
		if err != nil {
			return nil, "", errors.Wrap(err, "failed to tar typescript schemas")
		}
	}

	buildImage := b.buildImage
	_, rewritten, err := b.configStore.RewritePath(ctx, b.buildImage)
	if err != nil {
		return nil, "", errors.Wrap(err, "failed to rewrite build image")
	}
	if rewritten != "" {
		buildImage = rewritten
	}

	// The build runs in the function's original path so that relative deps
	// resolve, and leaves one /fn_<arch> tree per target architecture.
	fnPath := "/" + filepath.ToSlash(c.FunctionPath)
	var tsSchemasPath string
	if hasTSSchemas {
		tsSchemasPath = "/" + filepath.ToSlash(tsSchemasRel)
	}

	npmArchitectures := make([]string, len(c.Architectures))
	for i, a := range c.Architectures {
		npmArchitectures[i], err = npmArchitecture(a)
		if err != nil {
			return nil, "", err
		}
	}

	opts := []docker.StartContainerOption{
		docker.StartWithCopyFiles(fnTar, "/"),
		docker.StartWithEnv(
			"ARCHS="+strings.Join(npmArchitectures, " "),
			"SCHEMAS_PATH="+tsSchemasPath,
			"TSC_VERSION="+typescriptCompilerVersion,
		),
		docker.StartWithCommand([]string{"sh", "-c", typescriptBuildScript}),
		docker.StartWithWorkingDirectory(fnPath),
	}
	if schemasTar != nil {
		opts = append(opts, docker.StartWithCopyFiles(schemasTar, "/"))
	}

	cid, err := docker.StartContainer(ctx, "", buildImage, opts...)
	if err != nil {
		return nil, "", errors.Wrap(err, "failed to start typescript build container")
	}
	defer func() {
		// Use context.Background() so container cleanup happens even if ctx is cancelled.
		_ = docker.StopContainerByID(context.Background(), cid)
	}()

	if err := docker.WaitForContainerByID(ctx, cid); err != nil {
		return nil, "", errors.Wrap(err, "typescript build container failed")
	}

	// The script stages a single /fn_shared tree when nothing in the runtime
	// dependencies ships per-platform binaries, because every architecture would
	// otherwise get an identical copy. Streaming one tree instead of one per
	// architecture is worth more than the installs it also saves: each is around
	// 90MB for a project of any size.
	if shared, err := docker.TarFromContainer(ctx, cid, sharedFunctionPath); err == nil {
		ret := make(map[string][]byte, len(c.Architectures))
		for _, arch := range c.Architectures {
			ret[arch] = shared
		}
		return ret, sharedFunctionPath, nil
	}

	ret := make(map[string][]byte, len(c.Architectures))
	for _, arch := range c.Architectures {
		npmArch, _ := npmArchitecture(arch) // Ignore the error since we already did this once.
		ret[arch], err = docker.TarFromContainer(ctx, cid, fmt.Sprintf("/fn_%s", npmArch))
		if err != nil {
			return nil, "", errors.Wrapf(err, "failed to retrieve built function for architecture %s", arch)
		}
	}

	return ret, "", nil
}

// npmArchitecture maps an OCI architecture to the name npm expects for its
// --cpu flag, which follows Node's process.arch naming.
func npmArchitecture(a string) (string, error) {
	switch a {
	case "amd64":
		return "x64", nil
	case "arm64":
		return "arm64", nil
	default:
		return "", errors.Errorf("unable to determine npm architecture for architecture %s", a)
	}
}

// configureTypescriptImage sets the runtime configuration on the final image:
// the user, the function entrypoint and the gRPC port. The working directory is
// the architecture's own /fn_<arch> tree, so that Node resolves the node_modules
// built for this architecture.
// configureTypescriptImage sets the image's user, entrypoint, working
// directory and port. workDir overrides the per-architecture working directory
// when the build staged one tree for every architecture; it is empty otherwise.
func configureTypescriptImage(img v1.Image, arch, workDir string) (v1.Image, error) {
	cfgFile, err := img.ConfigFile()
	if err != nil {
		return nil, errors.Wrap(err, "failed to get config file")
	}
	cfg := cfgFile.Config

	npmArch, err := npmArchitecture(arch)
	if err != nil {
		return nil, err
	}
	cfg.Entrypoint = []string{"/nodejs/bin/node", "dist/main.js"}
	cfg.Cmd = nil
	cfg.WorkingDir = fmt.Sprintf("/fn_%s", npmArch)
	if workDir != "" {
		cfg.WorkingDir = workDir
	}
	// Set explicitly as well as selecting the :nonroot base, so an image
	// rewritten through spec.imageConfigs cannot quietly reintroduce root.
	// Matches the python builder.
	cfg.User = "nonroot:nonroot"
	if cfg.ExposedPorts == nil {
		cfg.ExposedPorts = map[string]struct{}{}
	}
	cfg.ExposedPorts["9443/tcp"] = struct{}{}

	return mutate.Config(img, cfg)
}

func newTypescriptBuilder(imageConfigs []pkgv1beta1.ImageConfig) *typescriptBuilder {
	return &typescriptBuilder{
		buildImage:   typescriptBuildImage,
		runtimeImage: typescriptRuntimeImage,
		transport:    http.DefaultTransport,
		configStore:  clixpkg.NewStaticImageConfigStore(imageConfigs),
	}
}
