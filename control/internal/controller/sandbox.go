package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"

	api "github.com/obscura/agentruntime/control/api/v1alpha1"
	"github.com/obscura/agentruntime/control/internal/backend"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const Finalizer = "sandbox.obscura.dev/runtime-cleanup"

type Reconciler struct {
	client.Client
	// Reader bypasses informer lag for creation intent and cleanup decisions.
	Reader       client.Reader
	Factory      backend.Factory
	PollInterval time.Duration
	Now          func() time.Time
}

func Config(c api.RuntimeConfig) backend.Config {
	return backend.Config{Type: c.Type, Endpoint: c.Endpoint, APIKeySecretName: c.APIKeySecretName, APIKeySecretKey: c.APIKeySecretKey}
}

func SpecHash(s api.SandboxSpec) string {
	encoded, _ := json.Marshal(s)
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

func Request(s *api.Sandbox) backend.Request {
	b := s.Status.Binding
	r := backend.Request{ID: b.ID, SpecHash: b.SpecHash, Template: b.Template, Network: backend.NetworkPolicy{AllowEgress: s.Spec.Network.AllowEgress, AllowHosts: s.Spec.Network.AllowHosts}, TimeoutSeconds: s.Spec.TimeoutSeconds}
	if v := s.Spec.Resources; v != nil {
		r.Resources = backend.Resources{VCPUs: uint32(v.VCPUs), MemoryMiB: uint32(v.MemoryMiB), DiskMiB: uint64(v.DiskMiB)}
	}
	return r
}

// +kubebuilder:rbac:groups=sandbox.obscura.dev,resources=sandboxes,verbs=get;list;watch;update;patch;delete
// +kubebuilder:rbac:groups=sandbox.obscura.dev,resources=sandboxes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=sandbox.obscura.dev,resources=sandboxes/finalizers,verbs=update
// +kubebuilder:rbac:groups=sandbox.obscura.dev,resources=sandboxruntimes,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get
func (r *Reconciler) Reconcile(ctx context.Context, q ctrl.Request) (ctrl.Result, error) {
	reader := r.Reader
	if reader == nil {
		reader = r.Client
	}
	s := &api.Sandbox{}
	if err := reader.Get(ctx, q.NamespacedName, s); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !s.DeletionTimestamp.IsZero() {
		return r.cleanup(ctx, s)
	}
	now := time.Now()
	if r.Now != nil {
		now = r.Now()
	}
	if s.Spec.TimeoutSeconds < 1 || s.Spec.TimeoutSeconds > 86400 || s.UID == "" {
		return r.report(ctx, s, "Failed", metav1.ConditionFalse, "InvalidSpec", "Invalid lifetime or sandbox identity", true)
	}
	expires := s.CreationTimestamp.Add(time.Duration(s.Spec.TimeoutSeconds) * time.Second)
	if !now.Before(expires) {
		return ctrl.Result{}, client.IgnoreNotFound(r.Delete(ctx, s))
	}
	if v := s.Spec.Resources; v != nil && (v.VCPUs < 1 || v.MemoryMiB < 1 || v.DiskMiB < 1) {
		return r.report(ctx, s, "Failed", metav1.ConditionFalse, "InvalidSpec", "Resource limits must be positive", true)
	}
	if !controllerutil.ContainsFinalizer(s, Finalizer) {
		before := s.DeepCopy()
		controllerutil.AddFinalizer(s, Finalizer)
		if err := r.Patch(ctx, s, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: time.Millisecond}, nil
	}
	if s.Status.Binding == nil {
		rt := &api.SandboxRuntime{}
		if err := reader.Get(ctx, client.ObjectKey{Namespace: s.Namespace, Name: s.Spec.RuntimeRef}, rt); err != nil {
			return r.report(ctx, s, "Pending", metav1.ConditionFalse, "RuntimeUnavailable", "Runtime configuration is unavailable", true)
		}
		template, ok := rt.Spec.Templates[s.Spec.TemplateRef]
		if !ok || template == "" {
			return r.report(ctx, s, "Failed", metav1.ConditionFalse, "UnknownTemplate", "Template is not registered in the selected runtime", false)
		}
		before := s.DeepCopy()
		s.Status.Binding = &api.RuntimeBinding{RuntimeUID: string(rt.UID), Config: *rt.Spec.Config.DeepCopy(), Template: template, ID: string(s.UID), SpecHash: SpecHash(s.Spec)}
		deadline := metav1.NewTime(expires)
		s.Status.ExpiresAt = &deadline
		s.Status.Phase = "Pending"
		if err := r.patchStatus(ctx, before, s); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: time.Millisecond}, nil
	}
	if s.Status.Binding.SpecHash != SpecHash(s.Spec) {
		return r.report(ctx, s, "Failed", metav1.ConditionFalse, "SpecChanged", "Launch specification changed; create a new sandbox", false)
	}
	b, err := r.Factory.Open(ctx, s.Namespace, Config(s.Status.Binding.Config))
	if err != nil {
		if errors.Is(err, backend.ErrAuth) {
			return r.report(ctx, s, "Unknown", metav1.ConditionUnknown, "CredentialsUnavailable", "Runtime credentials or permissions unavailable", true)
		}
		if errors.Is(err, backend.ErrInvalid) || errors.Is(err, backend.ErrUnsupported) {
			return r.report(ctx, s, "Failed", metav1.ConditionFalse, "RuntimeConfigurationInvalid", "Pinned runtime configuration is invalid", false)
		}
		return r.report(ctx, s, "Unknown", metav1.ConditionUnknown, "RuntimeUnavailable", "Runtime connection or credentials unavailable", true)
	}
	defer b.Close()
	request := Request(s)
	if err := b.Validate(request); err != nil {
		return r.report(ctx, s, "Failed", metav1.ConditionFalse, "UnsupportedRequest", "Runtime cannot satisfy the requested template, resources or policy", false)
	}
	var observed backend.Observation
	if s.Status.Binding.Handle == "" {
		if !s.Status.Binding.CreationStarted {
			before := s.DeepCopy()
			s.Status.Binding.CreationStarted = true
			s.Status.Phase = "Creating"
			if err := r.patchStatus(ctx, before, s); err != nil {
				return ctrl.Result{}, err
			}
			request.AllowCreate = true
		}
		observed, err = b.Ensure(ctx, request)
	} else {
		observed, err = b.Observe(ctx, s.Status.Binding.Handle)
	}
	if err != nil {
		switch {
		case errors.Is(err, backend.ErrAmbiguous):
			return r.report(ctx, s, "Unknown", metav1.ConditionUnknown, "CreationUnknown", "Creation outcome is unknown; waiting for backend discovery", true)
		case errors.Is(err, backend.ErrNotFound):
			return r.report(ctx, s, "Failed", metav1.ConditionFalse, "InstanceLost", "Previously bound instance is absent; automatic recreation is disabled", true)
		case errors.Is(err, backend.ErrConflict):
			return r.report(ctx, s, "Failed", metav1.ConditionFalse, "BackendConflict", "Backend rejected the stable identity or launch specification", false)
		case errors.Is(err, backend.ErrPrecondition):
			return r.report(ctx, s, "Failed", metav1.ConditionFalse, "BackendPrecondition", "Backend requires operator action before this identity can proceed", false)
		case errors.Is(err, backend.ErrInvalid):
			return r.report(ctx, s, "Failed", metav1.ConditionFalse, "InvalidRequest", "Backend rejected the launch request", false)
		case errors.Is(err, backend.ErrUnsupported):
			return r.report(ctx, s, "Failed", metav1.ConditionFalse, "UnsupportedRequest", "Backend does not support this operation", false)
		case errors.Is(err, backend.ErrAuth):
			return r.report(ctx, s, "Unknown", metav1.ConditionUnknown, "CredentialsUnavailable", "Runtime credentials or permissions unavailable", true)
		case errors.Is(err, backend.ErrRateLimited):
			return r.report(ctx, s, "Unknown", metav1.ConditionUnknown, "RateLimited", "Runtime request was rate limited", true)
		case errors.Is(err, backend.ErrCapacity):
			return r.report(ctx, s, "Failed", metav1.ConditionFalse, "CapacityUnavailable", "Runtime has no capacity for this sandbox", true)
		default:
			return r.report(ctx, s, "Unknown", metav1.ConditionUnknown, "RuntimeUnavailable", "Runtime observation unavailable; existing binding retained", true)
		}
	}
	if observed.Handle == "" {
		return r.report(ctx, s, "Unknown", metav1.ConditionUnknown, "InvalidResponse", "Runtime returned an empty instance handle", true)
	}
	before := s.DeepCopy()
	s.Status.Binding.Handle = observed.Handle
	state := metav1.ConditionFalse
	reason := "Runtime" + string(observed.State)
	if observed.State == backend.Ready {
		state = metav1.ConditionTrue
	}
	r.condition(s, string(observed.State), state, reason, "Runtime reported "+string(observed.State))
	if err := r.patchStatus(ctx, before, s); err != nil {
		return ctrl.Result{}, err
	}
	return r.next(now, expires), nil
}

func (r *Reconciler) cleanup(ctx context.Context, s *api.Sandbox) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(s, Finalizer) {
		return ctrl.Result{}, nil
	}
	if binding := s.Status.Binding; binding != nil && binding.CreationStarted {
		b, err := r.Factory.Open(ctx, s.Namespace, Config(binding.Config))
		if err != nil {
			if errors.Is(err, backend.ErrAuth) {
				return r.report(ctx, s, "Terminating", metav1.ConditionUnknown, "CredentialsUnavailable", "Runtime credentials or permissions unavailable during cleanup", true)
			}
			return r.report(ctx, s, "Terminating", metav1.ConditionUnknown, "CleanupBlocked", "Runtime unavailable during cleanup", true)
		}
		defer b.Close()
		handle := binding.Handle
		if handle == "" {
			if binding.Config.Type == "obscura" || binding.Config.Type == "native" {
				handle = binding.ID
			} else {
				request := Request(s)
				request.AllowCreate = false
				observed, err := b.Ensure(ctx, request)
				// A native stable ID is also the cleanup handle; do not create just to delete.
				if errors.Is(err, backend.ErrNotFound) {
					handle = ""
				} else if err != nil {
					if errors.Is(err, backend.ErrPrecondition) {
						return r.report(ctx, s, "Terminating", metav1.ConditionUnknown, "CleanupBlocked", "Backend requires operator action before cleanup", true)
					}
					return r.report(ctx, s, "Terminating", metav1.ConditionUnknown, "CreationUnknown", "Cannot prove resources absent after an uncertain creation", true)
				} else {
					handle = observed.Handle
				}
			}
		}
		if handle != "" {
			done, err := b.Delete(ctx, handle)
			if err != nil || !done {
				if errors.Is(err, backend.ErrPrecondition) {
					return r.report(ctx, s, "Terminating", metav1.ConditionUnknown, "CleanupBlocked", "Backend requires operator action before cleanup", true)
				}
				return r.report(ctx, s, "Terminating", metav1.ConditionUnknown, "CleanupPending", "Waiting for backend resource cleanup", true)
			}
		}
	}
	before := s.DeepCopy()
	controllerutil.RemoveFinalizer(s, Finalizer)
	return ctrl.Result{}, r.Patch(ctx, s, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
}

func (r *Reconciler) condition(s *api.Sandbox, phase string, value metav1.ConditionStatus, reason, message string) {
	s.Status.Phase = phase
	s.Status.ObservedGeneration = s.Generation
	meta.SetStatusCondition(&s.Status.Conditions, metav1.Condition{Type: "Ready", Status: value, ObservedGeneration: s.Generation, Reason: reason, Message: message})
}
func (r *Reconciler) patchStatus(ctx context.Context, before, s *api.Sandbox) error {
	if reflect.DeepEqual(before.Status, s.Status) {
		return nil
	}
	return r.Status().Patch(ctx, s, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
}
func (r *Reconciler) report(ctx context.Context, s *api.Sandbox, phase string, value metav1.ConditionStatus, reason, message string, retry bool) (ctrl.Result, error) {
	before := s.DeepCopy()
	r.condition(s, phase, value, reason, message)
	if err := r.patchStatus(ctx, before, s); err != nil {
		return ctrl.Result{}, err
	}
	now := time.Now()
	if r.Now != nil {
		now = r.Now()
	}
	if retry {
		if s.DeletionTimestamp.IsZero() && s.Spec.TimeoutSeconds >= 1 && s.Spec.TimeoutSeconds <= 86400 {
			expires := s.CreationTimestamp.Add(time.Duration(s.Spec.TimeoutSeconds) * time.Second)
			if now.Before(expires) {
				return r.next(now, expires), nil
			}
		}
		return ctrl.Result{RequeueAfter: r.interval()}, nil
	}
	// Even rejected requests must wake at expiry for lifecycle cleanup.
	return r.next(now, s.CreationTimestamp.Add(time.Duration(s.Spec.TimeoutSeconds)*time.Second)), nil
}
func (r *Reconciler) interval() time.Duration {
	if r.PollInterval > 0 {
		return r.PollInterval
	}
	return 5 * time.Second
}
func (r *Reconciler) next(now, expires time.Time) ctrl.Result {
	d := r.interval()
	if remaining := expires.Sub(now); remaining < d {
		d = remaining
	}
	if d <= 0 {
		d = time.Millisecond
	}
	return ctrl.Result{RequeueAfter: d}
}

func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Factory == nil {
		return fmt.Errorf("runtime factory is required")
	}
	if r.Reader == nil {
		r.Reader = mgr.GetAPIReader()
	}
	return ctrl.NewControllerManagedBy(mgr).For(&api.Sandbox{}).Complete(r)
}
