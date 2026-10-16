package backend

import (
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestHTTPProfilesKeepProviderWireMetadataInOnePlace(t *testing.T) {
	tests := []struct {
		name       string
		profile    httpRuntimeProfile
		authHeader string
		protocol   string
		admitted   bool
	}{
		{"e2b", e2bHTTPProfile(""), "X-API-Key", "ndjson", true},
		{"agentenv", agentENVHTTPProfile("v0.2.3"), "X-API-Key", "connect-envd", false},
		{"opensandbox", openSandboxHTTPProfile("release-1.1.1"), "OPEN-SANDBOX-API-KEY", "sse", false},
		{"cubesandbox", cubeSandboxHTTPProfile("v0.7.3"), "X-API-Key", "cube-exec", false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.profile.validate(); err != nil {
				t.Fatal(err)
			}
			if test.profile.AuthHeader != test.authHeader || test.profile.ExecuteProtocol != test.protocol || test.profile.Admitted != test.admitted {
				t.Fatalf("profile=%+v", test.profile)
			}
			req := httptest.NewRequest("GET", "http://runtime.invalid", nil)
			test.profile.auth(req, "secret")
			if req.Header.Get(test.authHeader) != "secret" {
				t.Fatalf("missing %s authentication header", test.authHeader)
			}
		})
	}
}

func TestHTTPProfileLookupRejectsUnknownProvider(t *testing.T) {
	if _, err := httpProfileFor("unknown", ""); err == nil {
		t.Fatal("unknown profile accepted")
	}
}

func TestAgentENVProfileMatchesPinnedLifecycleAndDataBoundaries(t *testing.T) {
	profile := agentENVHTTPProfile("v0.2.3")
	if profile.DiscoveryPath != "/v2/sandboxes" || profile.CreatePath != "/v2/sandboxes" {
		t.Fatalf("unexpected v2 lifecycle paths: %+v", profile)
	}
	if got := profile.SandboxPath("sandbox"); got != "/sandboxes/sandbox" {
		t.Fatalf("sandbox lifecycle path = %q", got)
	}
	if profile.ExecuteProtocol != "connect-envd" || profile.FileProtocol != "envd-http" {
		t.Fatalf("unexpected AgentENV data protocols: %+v", profile)
	}
	if got := profile.ExecutePath("sandbox"); got != "/process.Process/Start" {
		t.Fatalf("execute path = %q", got)
	}
	if got := profile.FilesPath("sandbox"); got != "/files" {
		t.Fatalf("files path = %q", got)
	}
	if want := map[string]bool{"execute": true, "files": true}; !reflect.DeepEqual(profile.Capabilities, want) {
		t.Fatalf("capabilities = %v, want only implemented capabilities %v", profile.Capabilities, want)
	}
}
