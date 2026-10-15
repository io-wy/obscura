package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"time"

	api "github.com/obscura/agentruntime/control/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"
)

type TenantReconciler struct {
	client.Client
	Reader            client.Reader
	AuthNamespace     string
	Now               func() time.Time
	OrphanGracePeriod time.Duration
}

// +kubebuilder:rbac:groups=sandbox.obscura.dev,resources=sandboxtenants,verbs=get;list;watch
// +kubebuilder:rbac:groups=sandbox.obscura.dev,resources=sandboxtenants/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=sandbox.obscura.dev,resources=sandboxes,verbs=get
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
func (r *TenantReconciler) Reconcile(ctx context.Context, q ctrl.Request) (ctrl.Result, error) {
	tenant := &api.SandboxTenant{}
	if err := r.Get(ctx, client.ObjectKey{Name: q.Name}, tenant); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	reader := r.Reader
	if reader == nil {
		reader = r.Client
	}
	if r.AuthNamespace == "" {
		return ctrl.Result{}, fmt.Errorf("tenant reconciler authentication namespace is required")
	}
	now := time.Now()
	if r.Now != nil {
		now = r.Now()
	}
	grace := r.OrphanGracePeriod
	if grace <= 0 {
		grace = time.Minute
	}

	ready, reason, message := metav1.ConditionTrue, "Ready", "Tenant credentials and namespace are valid"
	hashes := []string{}
	tenants := &api.SandboxTenantList{}
	if err := reader.List(ctx, tenants); err != nil {
		return ctrl.Result{}, err
	}
	for i := range tenants.Items {
		other := &tenants.Items[i]
		if other.Name != tenant.Name && other.DeletionTimestamp.IsZero() && other.Spec.Namespace == tenant.Spec.Namespace {
			ready, reason, message = metav1.ConditionFalse, "DuplicateNamespace", "Tenant namespace is claimed by more than one tenant"
			break
		}
	}
	if ready == metav1.ConditionTrue {
		seen := map[string]bool{}
		for _, selector := range tenant.Spec.Credentials {
			secret := &corev1.Secret{}
			err := reader.Get(ctx, client.ObjectKey{Namespace: r.AuthNamespace, Name: selector.Name}, secret)
			if err != nil {
				if apierrors.IsNotFound(err) {
					ready, reason, message = metav1.ConditionFalse, "CredentialsUnavailable", "One or more tenant credentials are unavailable"
					break
				}
				return ctrl.Result{}, err
			}
			value := secret.Data[selector.Key]
			if len(value) == 0 {
				ready, reason, message = metav1.ConditionFalse, "CredentialsUnavailable", "One or more tenant credentials are unavailable"
				break
			}
			sum := sha256.Sum256(value)
			hash := hex.EncodeToString(sum[:])
			if !seen[hash] {
				hashes = append(hashes, hash)
				seen[hash] = true
			}
		}
	}
	if ready != metav1.ConditionTrue {
		hashes = nil
	}
	sort.Strings(hashes)

	before := tenant.DeepCopy()
	tenant.Status.Sessions = liveSessions(tenant.Status.Sessions, now)
	allocations := tenant.Status.Allocations[:0]
	for _, allocation := range tenant.Status.Allocations {
		if now.Sub(allocation.ReservedAt.Time) < grace {
			allocations = append(allocations, allocation)
			continue
		}
		sandbox := &api.Sandbox{}
		err := reader.Get(ctx, client.ObjectKey{Namespace: tenant.Spec.Namespace, Name: allocation.SandboxID}, sandbox)
		if err == nil {
			allocations = append(allocations, allocation)
			continue
		}
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
	}
	tenant.Status.Allocations = allocations
	tenant.Status.CredentialHashes = hashes
	tenant.Status.ObservedGeneration = tenant.Generation
	meta.SetStatusCondition(&tenant.Status.Conditions, metav1.Condition{
		Type: "Ready", Status: ready, ObservedGeneration: tenant.Generation, Reason: reason, Message: message,
	})
	if err := r.Status().Patch(ctx, tenant, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: nextTenantSweep(tenant, now, grace)}, nil
}

func liveSessions(sessions []api.TenantSessionLease, now time.Time) []api.TenantSessionLease {
	result := sessions[:0]
	for _, session := range sessions {
		if now.Before(session.ExpiresAt.Time) {
			result = append(result, session)
		}
	}
	return result
}

func nextTenantSweep(tenant *api.SandboxTenant, now time.Time, grace time.Duration) time.Duration {
	next := grace
	for _, allocation := range tenant.Status.Allocations {
		if remaining := allocation.ReservedAt.Add(grace).Sub(now); remaining > 0 && remaining < next {
			next = remaining
		}
	}
	for _, session := range tenant.Status.Sessions {
		if remaining := session.ExpiresAt.Sub(now); remaining > 0 && remaining < next {
			next = remaining
		}
	}
	if next <= 0 {
		return time.Millisecond
	}
	return next
}

func (r *TenantReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Client == nil {
		r.Client = mgr.GetClient()
	}
	if r.Reader == nil {
		r.Reader = mgr.GetAPIReader()
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&api.SandboxTenant{}).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, object client.Object) []ctrlreconcile.Request {
			if object.GetNamespace() != r.AuthNamespace {
				return nil
			}
			tenants := &api.SandboxTenantList{}
			if err := r.List(ctx, tenants); err != nil {
				return nil
			}
			requests := []ctrlreconcile.Request{}
			for i := range tenants.Items {
				for _, selector := range tenants.Items[i].Spec.Credentials {
					if selector.Name == object.GetName() {
						requests = append(requests, ctrlreconcile.Request{NamespacedName: client.ObjectKey{Name: tenants.Items[i].Name}})
						break
					}
				}
			}
			return requests
		})).
		Complete(r)
}
