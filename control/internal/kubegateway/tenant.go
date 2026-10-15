package kubegateway

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"

	api "github.com/obscura/agentruntime/control/api/v1alpha1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var (
	ErrUnauthenticated   = errors.New("request is not authenticated")
	ErrTenantUnavailable = errors.New("tenant registry is unavailable")
)

type TenantResolver interface {
	Resolve(context.Context, string) (*api.SandboxTenant, error)
}

type KubernetesTenantResolver struct {
	Reader client.Reader
}

func (r *KubernetesTenantResolver) Resolve(ctx context.Context, key string) (*api.SandboxTenant, error) {
	if key == "" {
		return nil, ErrUnauthenticated
	}
	if r == nil || r.Reader == nil {
		return nil, ErrTenantUnavailable
	}
	sum := sha256.Sum256([]byte(key))
	want := hex.EncodeToString(sum[:])
	tenants := &api.SandboxTenantList{}
	if err := r.Reader.List(ctx, tenants); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrTenantUnavailable, err)
	}
	namespaceOwners := map[string]int{}
	for i := range tenants.Items {
		if tenants.Items[i].DeletionTimestamp.IsZero() {
			namespaceOwners[tenants.Items[i].Spec.Namespace]++
		}
	}
	var match *api.SandboxTenant
	for i := range tenants.Items {
		tenant := &tenants.Items[i]
		condition := meta.FindStatusCondition(tenant.Status.Conditions, "Ready")
		if condition == nil || condition.Status != metav1.ConditionTrue || !tenant.DeletionTimestamp.IsZero() || namespaceOwners[tenant.Spec.Namespace] != 1 {
			continue
		}
		for _, hash := range tenant.Status.CredentialHashes {
			if subtle.ConstantTimeCompare([]byte(hash), []byte(want)) != 1 {
				continue
			}
			if match != nil {
				return nil, ErrUnauthenticated
			}
			match = tenant.DeepCopy()
			break
		}
	}
	if match == nil {
		return nil, ErrUnauthenticated
	}
	return match, nil
}

type tenantContextKey struct{}

func tenantFromContext(ctx context.Context) *api.SandboxTenant {
	tenant, _ := ctx.Value(tenantContextKey{}).(*api.SandboxTenant)
	return tenant
}
