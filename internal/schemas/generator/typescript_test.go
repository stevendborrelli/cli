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

package generator

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/spf13/afero"
)

func TestCollectOpenAPISchemas(t *testing.T) {
	inputFS := afero.NewBasePathFs(afero.FromIOFS{FS: testdataJSONFS}, "testdata")

	schemas, err := typescriptGenerator{}.collectOpenAPISchemas(inputFS)
	if err != nil {
		t.Fatal(err)
	}

	// These come from different testdata files (the core API and the
	// resource.k8s.io group), so finding both confirms multiple documents
	// were merged into one map rather than only the last one read.
	for _, id := range []string{
		"io.k8s.api.core.v1.Pod",
		"io.k8s.apimachinery.pkg.apis.meta.v1.ObjectMeta",
	} {
		if _, ok := schemas[id]; !ok {
			t.Errorf("collectOpenAPISchemas() did not return %q", id)
		}
	}
}

func TestToLegacyOpenAPIDefinitions(t *testing.T) {
	inputFS := afero.NewBasePathFs(afero.FromIOFS{FS: testdataJSONFS}, "testdata")

	schemas, err := typescriptGenerator{}.collectOpenAPISchemas(inputFS)
	if err != nil {
		t.Fatal(err)
	}

	out, err := toLegacyOpenAPIDefinitions(schemas)
	if err != nil {
		t.Fatal(err)
	}

	if strings.Contains(string(out), "#/components/schemas/") {
		t.Error("toLegacyOpenAPIDefinitions() did not rewrite all #/components/schemas/ refs to #/definitions/")
	}

	var doc struct {
		Definitions map[string]json.RawMessage `json:"definitions"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("toLegacyOpenAPIDefinitions() did not produce a {\"definitions\": ...} document: %v", err)
	}

	// A ref inside a schema that survived the rewrite should now point at
	// "#/definitions/", the only prefix openapi-generate's ref resolution
	// (kubernetes-models-ts, v6.1.1) understands.
	pod, ok := doc.Definitions["io.k8s.api.core.v1.Pod"]
	if !ok {
		t.Fatal("toLegacyOpenAPIDefinitions() dropped io.k8s.api.core.v1.Pod")
	}
	if !strings.Contains(string(pod), `"#/definitions/io.k8s.apimachinery.pkg.apis.meta.v1.ObjectMeta"`) {
		t.Errorf("Pod schema ref was not rewritten to #/definitions/: %s", pod)
	}

	// DeleteOptions is registered under every API group's version as a
	// generic meta kind, so it has more than one
	// x-kubernetes-group-version-kind entry. That combination trips a bug in
	// openapi-generate's alias output (see toLegacyOpenAPIDefinitions), so
	// its GVK extension must be stripped entirely.
	deleteOptions, ok := doc.Definitions["io.k8s.apimachinery.pkg.apis.meta.v1.DeleteOptions"]
	if !ok {
		t.Fatal("toLegacyOpenAPIDefinitions() dropped io.k8s.apimachinery.pkg.apis.meta.v1.DeleteOptions")
	}
	if strings.Contains(string(deleteOptions), "x-kubernetes-group-version-kind") {
		t.Error("toLegacyOpenAPIDefinitions() did not strip the multi-GVK extension from DeleteOptions")
	}

	// A schema with at most one GVK is unaffected by the bug and must keep
	// its extension, since it is what lets openapi-generate populate the
	// apiVersion/kind enum on the generated type.
	pod = doc.Definitions["io.k8s.api.core.v1.Pod"]
	if !strings.Contains(string(pod), "x-kubernetes-group-version-kind") {
		t.Error("toLegacyOpenAPIDefinitions() incorrectly stripped the GVK extension from Pod, which has only one")
	}
}
