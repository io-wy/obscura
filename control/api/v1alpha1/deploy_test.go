package v1alpha1

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	yamlutil "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/kube-openapi/pkg/validation/spec"
	"k8s.io/kube-openapi/pkg/validation/strfmt"
	"k8s.io/kube-openapi/pkg/validation/validate"
	"sigs.k8s.io/yaml"
)

// Rendering alone does not catch missing CRDs or RBAC: both used to render
// successfully while every tenant-authenticated request failed in a real cluster.
func TestDeploymentsSupportTenantAuthentication(t *testing.T) {
	if _, err := exec.LookPath("kubectl"); err != nil {
		t.Skip("kubectl is required for deployment rendering (also checked by make test-manifests)")
	}
	for _, path := range []string{"deploy", "deploy/overlays/native", "deploy/overlays/kind"} {
		t.Run(path, func(t *testing.T) {
			output, err := exec.Command("kubectl", "kustomize", "../../../"+path).CombinedOutput()
			if err != nil {
				t.Fatalf("render: %v\n%s", err, output)
			}
			objects := map[string]map[string]interface{}{}
			decoder := yamlutil.NewYAMLOrJSONDecoder(bytes.NewReader(output), 4096)
			for {
				var object unstructured.Unstructured
				if err := decoder.Decode(&object); err == io.EOF {
					break
				} else if err != nil {
					t.Fatal(err)
				}
				objects[object.GetKind()+"/"+object.GetName()] = object.Object
			}
			if objects["CustomResourceDefinition/sandboxtenants.sandbox.obscura.dev"] == nil {
				t.Fatal("SandboxTenant CRD is missing from deployment")
			}
			permissions := map[string]map[string][]string{
				"obscura-controller": {
					"sandboxes":        {"get", "list", "watch", "patch", "delete"},
					"sandboxes/status": {"patch"}, "sandboxruntimes": {"get", "list", "watch"},
					"sandboxtenants": {"get", "list", "watch"}, "sandboxtenants/status": {"get", "patch"},
					"secrets": {"get", "list", "watch"},
				},
				"obscura-gateway": {
					"sandboxes": {"get", "create", "delete"}, "sandboxtenants": {"get", "list"},
					"sandboxtenants/status": {"get", "patch"}, "secrets": {"get"},
				},
			}
			for name, resources := range permissions {
				var role rbacv1.ClusterRole
				if err := runtime.DefaultUnstructuredConverter.FromUnstructured(objects["ClusterRole/"+name], &role); err != nil {
					t.Fatal(err)
				}
				for resource, verbs := range resources {
					group := GroupVersion.Group
					if resource == "secrets" {
						group = ""
					}
					for _, verb := range verbs {
						if !roleGrants(role.Rules, group, resource, verb) {
							t.Errorf("%s lacks cluster-wide %s on %s", name, verb, resource)
						}
					}
				}
				var binding rbacv1.ClusterRoleBinding
				if err := runtime.DefaultUnstructuredConverter.FromUnstructured(objects["ClusterRoleBinding/"+name], &binding); err != nil {
					t.Fatal(err)
				}
				if binding.RoleRef.Kind != "ClusterRole" || binding.RoleRef.Name != name || len(binding.Subjects) != 1 || binding.Subjects[0].Kind != "ServiceAccount" || binding.Subjects[0].Name != name || binding.Subjects[0].Namespace != "obscura-system" {
					t.Errorf("invalid shared-service binding: %#v", binding)
				}
			}
		})
	}
}

func roleGrants(rules []rbacv1.PolicyRule, group, resource, verb string) bool {
	contains := func(values []string, want string) bool {
		for _, value := range values {
			if value == want || value == "*" {
				return true
			}
		}
		return false
	}
	for _, rule := range rules {
		if len(rule.ResourceNames) == 0 && contains(rule.APIGroups, group) && contains(rule.Resources, resource) && contains(rule.Verbs, verb) {
			return true
		}
	}
	return false
}

func TestTenantDeploymentExampleMatchesSchema(t *testing.T) {
	data, err := os.ReadFile("../../../deploy/crds/sandbox.obscura.dev_sandboxtenants.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var crd extv1.CustomResourceDefinition
	if err := yaml.UnmarshalStrict(data, &crd); err != nil {
		t.Fatal(err)
	}
	schemaJSON, err := json.Marshal(crd.Spec.Versions[0].Schema.OpenAPIV3Schema)
	if err != nil {
		t.Fatal(err)
	}
	var openAPI spec.Schema
	if err := json.Unmarshal(schemaJSON, &openAPI); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile("../../../deploy/base/tenant.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var object unstructured.Unstructured
	if err := yamlutil.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096).Decode(&object); err != nil {
		t.Fatal(err)
	}
	if object.GetKind() != "SandboxTenant" || object.GetNamespace() != "" {
		t.Fatal("tenant example must be a cluster-scoped SandboxTenant")
	}
	// Use Kubernetes' OpenAPI validator without a live server. CEL transition
	// rules (including namespace immutability) are covered by envtest.
	if err := validate.AgainstSchema(&openAPI, object.Object, strfmt.Default); err != nil {
		t.Fatal(err)
	}
}
