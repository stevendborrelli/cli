//go:build dockergate

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

// This test runs the real npm toolchain in a build container, pulls the real
// distroless runtime image over the network, and, for the host's own
// architecture, runs the built image itself, so it can't run in the
// hermetic Nix sandbox that runs our unit tests -- run it locally with a
// Docker daemon available:
//
//	go test -tags dockergate ./internal/project/functions/... -run TestTypeScriptBuildMultiArch -v
package functions

import (
	"archive/tar"
	"context"
	"embed"
	"io"
	"os/exec"
	"path"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/daemon"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/spf13/afero"
)

//go:embed testdata/typescript-function/**
var typescriptFunction embed.FS

// TestTypeScriptBuildMultiArch builds a real TypeScript function fixture --
// with a real npm dependency, not just a stub -- for both amd64 and arm64,
// using the production builder and its default (real) runtime base image.
// It checks the concrete claims a mixed CRD+OpenAPI review raised about this
// path: that each architecture's image has its own dist/main.js and
// node_modules, ships without devDependencies or file: paths, runs as
// nonroot with the documented entrypoint, and -- for whichever architecture
// matches the host, so no emulation is required -- actually starts and
// resolves its dependency at runtime.
func TestTypeScriptBuildMultiArch(t *testing.T) {
	archs := []string{"amd64", "arm64"}

	b := newTypeScriptBuilder(nil)

	fnImgs, err := b.Build(context.Background(), BuildContext{
		ProjectFS:     afero.FromIOFS{FS: typescriptFunction},
		FunctionPath:  "testdata/typescript-function",
		Architectures: archs,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(fnImgs), len(archs); got != want {
		t.Fatalf("len(fnImgs) = %d, want %d", got, want)
	}

	for i, arch := range archs {
		img := fnImgs[i]
		npmArch := "x64"
		if arch == "arm64" {
			npmArch = "arm64"
		}
		fnDir := "/fn_" + npmArch

		t.Run(arch+"/image config", func(t *testing.T) {
			cfgFile, err := img.ConfigFile()
			if err != nil {
				t.Fatal(err)
			}
			cfg := cfgFile.Config
			if cfg.User != "nonroot:nonroot" {
				t.Errorf("User = %q, want nonroot:nonroot", cfg.User)
			}
			if want := []string{"/nodejs/bin/node", "dist/main.js"}; !slices.Equal(cfg.Entrypoint, want) {
				t.Errorf("Entrypoint = %v, want %v", cfg.Entrypoint, want)
			}
			if cfg.WorkingDir != fnDir {
				t.Errorf("WorkingDir = %q, want %q", cfg.WorkingDir, fnDir)
			}
			if _, ok := cfg.ExposedPorts["9443/tcp"]; !ok {
				t.Errorf("ExposedPorts = %v, want 9443/tcp exposed", cfg.ExposedPorts)
			}
		})

		t.Run(arch+"/image contents", func(t *testing.T) {
			mustExistInImage(t, img, path.Join(fnDir, "dist", "main.js"))
			mustExistInImage(t, img, path.Join(fnDir, "node_modules", "left-pad", "index.js"))
			pkgJSON := readFromImage(t, img, path.Join(fnDir, "package.json"))
			if strings.Contains(pkgJSON, "devDependencies") {
				t.Error("package.json in the final image still has devDependencies")
			}
			if strings.Contains(pkgJSON, "file:") {
				t.Error("package.json in the final image still has a file: dependency path")
			}
		})
	}

	// Actually running the container needs the host's own architecture: the
	// other one would need emulation this environment may not have, and a
	// failure there would be about the test environment, not the build.
	nativeIdx := slices.Index(archs, runtime.GOARCH)
	if nativeIdx < 0 {
		t.Logf("host architecture %s not covered by this test's archs %v; skipping the runtime check", runtime.GOARCH, archs)
		return
	}

	tag, err := name.NewTag("localhost/crossplane-ts-dockergate-test:latest")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := daemon.Write(tag, fnImgs[nativeIdx]); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = exec.Command("docker", "rmi", "-f", tag.String()).Run() //nolint:errcheck,gosec // Best-effort cleanup.
	})

	out, err := exec.CommandContext(context.Background(), "docker", "run", "--rm", tag.String()).CombinedOutput() //nolint:gosec // Fixed args; not user input.
	if err != nil {
		t.Fatalf("docker run failed: %v\noutput:\n%s", err, out)
	}
	if !strings.Contains(string(out), "***hi") {
		t.Errorf("container output = %q, want it to contain %q (left-pad resolved and ran)", out, "***hi")
	}
}

func mustExistInImage(t *testing.T, img v1.Image, imgPath string) {
	t.Helper()
	if _, ok := statInImage(t, img, imgPath); !ok {
		t.Errorf("missing expected file %s in image", imgPath)
	}
}

func readFromImage(t *testing.T, img v1.Image, imgPath string) string {
	t.Helper()
	content, ok := statInImage(t, img, imgPath)
	if !ok {
		t.Fatalf("missing expected file %s in image", imgPath)
	}
	return content
}

// statInImage returns a file's content from the image's flattened
// filesystem, and whether it was found. imgPath must not have a leading
// slash: mutate.Extract's tar entries are rooted at ".".
func statInImage(t *testing.T, img v1.Image, imgPath string) (string, bool) {
	t.Helper()
	imgPath = strings.TrimPrefix(imgPath, "/")

	rc := mutate.Extract(img)
	defer rc.Close() //nolint:errcheck // Best-effort close of a read-only stream.

	tr := tar.NewReader(rc)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return "", false
		}
		if err != nil {
			t.Fatal(err)
		}
		if strings.TrimPrefix(path.Clean(hdr.Name), "./") != imgPath {
			continue
		}
		content, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		return string(content), true
	}
}
