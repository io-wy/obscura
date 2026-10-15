package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var GroupVersion = schema.GroupVersion{Group: "sandbox.obscura.dev", Version: "v1alpha1"}

func AddToScheme(s *runtime.Scheme) error {
	s.AddKnownTypes(GroupVersion, &Sandbox{}, &SandboxList{}, &SandboxRuntime{}, &SandboxRuntimeList{})
	metav1.AddToGroupVersion(s, GroupVersion)
	return nil
}

// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="sandbox launch specification is immutable; create a new sandbox to change it"
type SandboxSpec struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	RuntimeRef string `json:"runtimeRef"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	TemplateRef string `json:"templateRef"`
	// Omit resources for providers whose templates fix the machine size.
	Resources *ResourceSpec `json:"resources,omitempty"`
	Network   NetworkPolicy `json:"network"`
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=86400
	TimeoutSeconds int64 `json:"timeoutSeconds"`
}

type ResourceSpec struct {
	// +kubebuilder:validation:Minimum=1
	VCPUs int32 `json:"vcpus"`
	// +kubebuilder:validation:Minimum=1
	MemoryMiB int32 `json:"memoryMiB"`
	// +kubebuilder:validation:Minimum=1
	DiskMiB int64 `json:"diskMiB"`
}

type NetworkPolicy struct {
	AllowEgress bool `json:"allowEgress"`
	// +kubebuilder:validation:MaxItems=128
	// +kubebuilder:validation:items:MaxLength=253
	AllowHosts []string `json:"allowHosts,omitempty"`
}

type RuntimeConfig struct {
	// +kubebuilder:validation:Enum=obscura;native;agentenv;cubesandbox;e2b
	Type string `json:"type"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=2048
	Endpoint         string `json:"endpoint"`
	APIKeySecretName string `json:"apiKeySecretName,omitempty"`
	APIKeySecretKey  string `json:"apiKeySecretKey,omitempty"`
}

type SandboxRuntimeSpec struct {
	Config RuntimeConfig `json:"config"`
	// Logical template names map to backend-specific immutable references or IDs.
	Templates map[string]string `json:"templates"`
}

type RuntimeBinding struct {
	RuntimeUID string `json:"runtimeUID"`
	// Pin the non-secret backend configuration so runtime edits/deletion cannot
	// redirect lifecycle operations away from the original instance.
	Config          RuntimeConfig `json:"config"`
	Template        string        `json:"template"`
	ID              string        `json:"id"`
	Handle          string        `json:"handle,omitempty"`
	SpecHash        string        `json:"specHash"`
	CreationStarted bool          `json:"creationStarted,omitempty"`
}

type SandboxStatus struct {
	Phase              string          `json:"phase,omitempty"`
	ObservedGeneration int64           `json:"observedGeneration,omitempty"`
	Binding            *RuntimeBinding `json:"binding,omitempty"`
	ExpiresAt          *metav1.Time    `json:"expiresAt,omitempty"`
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=sbx
// +kubebuilder:printcolumn:name="Runtime",type=string,JSONPath=`.spec.runtimeRef`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type Sandbox struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              SandboxSpec   `json:"spec"`
	Status            SandboxStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type SandboxList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Sandbox `json:"items"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:shortName=sbr
type SandboxRuntime struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              SandboxRuntimeSpec `json:"spec"`
}

// +kubebuilder:object:root=true
type SandboxRuntimeList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SandboxRuntime `json:"items"`
}
