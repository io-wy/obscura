package backend

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const operationTimeout = 15 * time.Second

type Registry struct{ reader client.Reader }

func NewRegistry(reader client.Reader) *Registry { return &Registry{reader: reader} }

// Open resolves credentials at the point of use. Only Secret references are stored in Config.
func (r *Registry) Open(ctx context.Context, namespace string, config Config) (Backend, error) {
	switch config.Type {
	case "native", "obscura":
		if config.APIKeySecretName != "" {
			return nil, fmt.Errorf("%w: native Unix sockets do not use API keys", ErrUnsupported)
		}
		return openNative(config.Endpoint)
	case "agentenv", "opensandbox", "cubesandbox":
		return nil, fmt.Errorf("%w: runtime provider is not admitted", ErrUnsupported)
	case "e2b":
		endpoint, err := url.Parse(config.LifecycleEndpoint())
		if err != nil || endpoint == nil || (endpoint.Scheme != "http" && endpoint.Scheme != "https") || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
			return nil, fmt.Errorf("%w: invalid HTTP runtime endpoint", ErrInvalid)
		}
		key := ""
		if config.APIKeySecretName != "" {
			if namespace == "" || r.reader == nil {
				return nil, fmt.Errorf("%w: namespaced Secret reader is required", ErrAuth)
			}
			field := config.APIKeySecretKey
			if field == "" {
				field = "api-key"
			}
			var secret corev1.Secret
			if err := r.reader.Get(ctx, types.NamespacedName{Namespace: namespace, Name: config.APIKeySecretName}, &secret); err != nil {
				category := ErrUnavailable
				if apierrors.IsNotFound(err) || apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err) {
					category = ErrAuth
				}
				return nil, fmt.Errorf("%w: load runtime API key Secret: %v", category, err)
			}
			if len(secret.Data[field]) == 0 {
				return nil, fmt.Errorf("%w: runtime API key Secret field is empty", ErrAuth)
			}
			key = string(secret.Data[field])
		} else if config.APIKeySecretKey != "" {
			return nil, fmt.Errorf("%w: API key Secret name is required", ErrAuth)
		}
		transport := http.DefaultTransport.(*http.Transport).Clone()
		dataEndpoint, err := url.Parse(config.ExecutionEndpoint())
		if err != nil || dataEndpoint == nil || (dataEndpoint.Scheme != "http" && dataEndpoint.Scheme != "https") || dataEndpoint.Host == "" || dataEndpoint.User != nil || dataEndpoint.RawQuery != "" || dataEndpoint.Fragment != "" {
			return nil, fmt.Errorf("%w: invalid HTTP data runtime endpoint", ErrInvalid)
		}
		return &httpBackendClient{endpoint: strings.TrimRight(endpoint.String(), "/"), dataEndpoint: strings.TrimRight(dataEndpoint.String(), "/"), key: key, client: &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
	default:
		return nil, fmt.Errorf("%w: unknown runtime type", ErrUnsupported)
	}
}
