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

// This test runs the real TypeScript toolchain in a container, so it can't
// run in the hermetic Nix sandbox that runs our unit tests -- run it locally
// with a Docker daemon available:
//
//	go test -tags dockergate ./internal/schemas/manager/... -run TestGenerateFromMultipleSourcesTypeScriptRealToolchain -v
package manager

import (
	"context"
	"embed"
	"strings"
	"testing"

	"github.com/spf13/afero"

	"github.com/crossplane/cli/v2/internal/schemas/generator"
	"github.com/crossplane/cli/v2/internal/schemas/runner"
)

//go:embed testdata/*.yaml testdata/*.json
var typescriptDockergateTestdataFS embed.FS

// dockergateOpenAPISource is a filesystem-backed OpenAPI source, mirroring
// the shape of k8sOpenAPISource without hitting the network.
type dockergateOpenAPISource struct {
	id string
	fs afero.Fs
}

func (s *dockergateOpenAPISource) ID() string { return s.id }

func (s *dockergateOpenAPISource) Version(_ context.Context) (string, error) { return "v1", nil }

func (s *dockergateOpenAPISource) Resources(_ context.Context) (afero.Fs, error) { return s.fs, nil }

func (s *dockergateOpenAPISource) Type() SourceType { return SourceTypeOpenAPI }

// tsGeneratorOnly returns just the typescript generator, so this test isn't
// slowed down generating every other language too.
func tsGeneratorOnly() generator.Interface {
	for _, g := range generator.AllLanguages() {
		if g.Language() == "typescript" {
			return g
		}
	}
	panic("no typescript generator")
}

// This is the real-toolchain counterpart to
// TestGenerateFromMultipleSources_MergesAcrossSourceTypes: that test proves
// the manager's merge dispatch with a mock generator; this proves the actual
// TypeScript generator's MergeGeneratedSchemas produces a correct, compilable
// package when a project has both a CRD/XRD source (its own APIs, or a CRD
// dependency) and an OpenAPI source (a k8s: dependency) -- the case a real
// project is likely to have.
func TestGenerateFromMultipleSourcesTypeScriptRealToolchain(t *testing.T) {
	testdataFS := afero.NewBasePathFs(afero.FromIOFS{FS: typescriptDockergateTestdataFS}, "testdata")

	crdSrc := NewFSSource("widget", testdataFS)
	openAPISrc := &dockergateOpenAPISource{id: "k8s://v1", fs: testdataFS}

	outFS := afero.NewMemMapFs()
	m := New(outFS, []generator.Interface{tsGeneratorOnly()}, runner.NewRealSchemaRunner(runner.WithImageConfig(nil)))

	if err := m.GenerateFromMultipleSources(t.Context(), []Source{crdSrc, openAPISrc}); err != nil {
		t.Fatal(err)
	}

	// The CRD-sourced kind and the OpenAPI-sourced kind must both be
	// reachable by subpath import (the form every generated function and
	// the README use), and both must be named in the root barrel (the "."
	// export) -- which is exactly what silently lost the CRD-sourced group
	// before MergeGeneratedSchemas existed.
	mustExist := []string{
		"typescript/package.json",
		"typescript/index.js",
		"typescript/index.d.ts",
		"typescript/example.com/v1/Widget.js",
		"typescript/v1/Namespace.js",
	}
	for _, p := range mustExist {
		if ok, err := afero.Exists(outFS, p); err != nil {
			t.Fatal(err)
		} else if !ok {
			t.Errorf("missing expected file %s", p)
		}
	}

	rootIndex, err := afero.ReadFile(outFS, "typescript/index.js")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(rootIndex), "example.com") {
		t.Errorf("root index.js does not re-export the CRD-sourced group example.com:\n%s", rootIndex)
	}
	if !strings.Contains(string(rootIndex), "./v1/index.js") {
		t.Errorf("root index.js does not re-export the OpenAPI-sourced group v1:\n%s", rootIndex)
	}
}
