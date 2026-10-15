package v1alpha1

import (
	"os"
	"path/filepath"
	"testing"

	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/yaml"
)

// A fake client does not validate CRD schemas. This validates the actual generated
// schema with Kubernetes structural validation even when envtest is unavailable.
// CEL admission and status/finalizer semantics are covered by the envtest suite.
func TestGeneratedCRDsAreStructural(t *testing.T) {
	files, err := filepath.Glob("../../../deploy/crds/sandbox.obscura.dev_*.yaml")
	if err != nil || len(files) != 3 {
		t.Fatalf("CRD files: %v %v", files, err)
	}
	for _, path := range files {
		t.Run(filepath.Base(path), func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var crd extv1.CustomResourceDefinition
			if err := yaml.UnmarshalStrict(data, &crd); err != nil {
				t.Fatal(err)
			}
			wantScope := extv1.NamespaceScoped
			if crd.Spec.Names.Kind == "SandboxTenant" {
				wantScope = extv1.ClusterScoped
			}
			if crd.Spec.Scope != wantScope || len(crd.Spec.Versions) != 1 {
				t.Fatalf("unexpected resource scope or versions: %s %d", crd.Spec.Scope, len(crd.Spec.Versions))
			}
			var internal apiextensions.JSONSchemaProps
			if err := extv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(crd.Spec.Versions[0].Schema.OpenAPIV3Schema, &internal, nil); err != nil {
				t.Fatal(err)
			}
			structural, err := schema.NewStructural(&internal)
			if err != nil {
				t.Fatal(err)
			}
			if errs := schema.ValidateStructural(field.NewPath("schema"), structural); len(errs) > 0 {
				t.Fatal(errs.ToAggregate())
			}
			if crd.Spec.Names.Kind == "Sandbox" {
				if crd.Spec.Versions[0].Subresources.Status == nil {
					t.Fatal("missing status subresource")
				}
				spec := internal.Properties["spec"]
				if len(spec.XValidations) != 1 || spec.XValidations[0].Rule != "self == oldSelf" {
					t.Fatal("immutable spec rule missing")
				}
			}
			if crd.Spec.Names.Kind == "SandboxTenant" && crd.Spec.Versions[0].Subresources.Status == nil {
				t.Fatal("tenant status subresource missing")
			}
		})
	}
}
