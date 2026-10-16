package backend

import (
	"fmt"
	"net/http"
	"strings"
)

// httpRuntimeProfile describes the provider-specific wire details used by the
// shared HTTP runtime client. Profiles deliberately contain policy and codecs,
// while transport, deadlines, limits, error mapping, and stream validation
// remain shared.
type httpRuntimeProfile struct {
	Name       string
	APIVersion string
	Admitted   bool

	AuthHeader string

	DiscoveryPath string
	CreatePath    string
	SandboxPath   func(string) string
	ExecutePath   func(string) string
	FilesPath     func(string) string
	PTYPath       func(string) string
	PortPath      func(string, uint16) string

	// Protocol fields are admission metadata until the corresponding codec is
	// implemented and covered by a provider contract test.
	ExecuteProtocol string
	FileProtocol    string
	Capabilities    map[string]bool
}

func e2bHTTPProfile(apiVersion string) httpRuntimeProfile {
	return httpRuntimeProfile{
		Name:            "e2b",
		APIVersion:      apiVersion,
		Admitted:        true,
		AuthHeader:      "X-API-Key",
		DiscoveryPath:   "/v2/sandboxes",
		CreatePath:      "/sandboxes",
		SandboxPath:     func(id string) string { return "/sandboxes/" + id },
		ExecutePath:     func(id string) string { return "/sandboxes/" + id + "/commands" },
		FilesPath:       func(id string) string { return "/sandboxes/" + id + "/files" },
		PTYPath:         func(id string) string { return "/sandboxes/" + id + "/pty" },
		PortPath:        func(id string, port uint16) string { return fmt.Sprintf("/sandboxes/%s/ports/%d", id, port) },
		ExecuteProtocol: "ndjson",
		FileProtocol:    "raw-http",
		Capabilities:    map[string]bool{"execute": true, "files": true, "pty": true, "ports": true},
	}
}

func agentENVHTTPProfile(apiVersion string) httpRuntimeProfile {
	profile := e2bHTTPProfile(apiVersion)
	profile.Name = "agentenv"
	profile.Admitted = false
	profile.DiscoveryPath = "/v2/sandboxes"
	profile.CreatePath = "/v2/sandboxes"
	// AgentENV v2 only versions list/create/connect. Get/delete remain on the
	// lifecycle path documented by the pinned OpenAPI contract.
	profile.SandboxPath = func(id string) string { return "/sandboxes/" + id }
	profile.ExecuteProtocol = "connect-envd"
	profile.FileProtocol = "envd-http"
	profile.ExecutePath = func(string) string { return "/process.Process/Start" }
	profile.FilesPath = func(string) string { return "/files" }
	profile.Capabilities = map[string]bool{"execute": true, "files": true}
	return profile
}

func openSandboxHTTPProfile(apiVersion string) httpRuntimeProfile {
	profile := e2bHTTPProfile(apiVersion)
	profile.Name = "opensandbox"
	profile.Admitted = false
	profile.AuthHeader = "OPEN-SANDBOX-API-KEY"
	profile.DiscoveryPath = "/v1/sandboxes"
	profile.CreatePath = "/v1/sandboxes"
	profile.SandboxPath = func(id string) string { return "/v1/sandboxes/" + id }
	profile.ExecutePath = func(string) string { return "/command" }
	profile.FilesPath = func(string) string { return "/files" }
	profile.ExecuteProtocol = "sse"
	profile.FileProtocol = "execd"
	profile.Capabilities = map[string]bool{"execute": true, "files": true, "pause_resume": true, "snapshot": true}
	return profile
}

func cubeSandboxHTTPProfile(apiVersion string) httpRuntimeProfile {
	profile := e2bHTTPProfile(apiVersion)
	profile.Name = "cubesandbox"
	profile.Admitted = false
	profile.ExecuteProtocol = "cube-exec"
	profile.FileProtocol = "cube-api"
	profile.Capabilities = map[string]bool{"pause_resume": true, "snapshot": true, "volumes": true}
	return profile
}

func httpProfileFor(provider, apiVersion string) (httpRuntimeProfile, error) {
	switch strings.ToLower(provider) {
	case "e2b":
		return e2bHTTPProfile(apiVersion), nil
	case "agentenv":
		return agentENVHTTPProfile(apiVersion), nil
	case "opensandbox":
		return openSandboxHTTPProfile(apiVersion), nil
	case "cubesandbox":
		return cubeSandboxHTTPProfile(apiVersion), nil
	default:
		return httpRuntimeProfile{}, fmt.Errorf("%w: unknown HTTP runtime provider %q", ErrUnsupported, provider)
	}
}

func (p httpRuntimeProfile) validate() error {
	if p.Name == "" || p.AuthHeader == "" || p.DiscoveryPath == "" || p.CreatePath == "" || p.SandboxPath == nil {
		return fmt.Errorf("%w: incomplete HTTP runtime profile", ErrUnsupported)
	}
	if p.ExecuteProtocol == "" || p.FileProtocol == "" {
		return fmt.Errorf("%w: HTTP runtime profile has no data-plane protocol", ErrUnsupported)
	}
	return nil
}

func (p httpRuntimeProfile) auth(req *http.Request, key string) {
	if key != "" {
		req.Header.Set(p.AuthHeader, key)
	}
}
