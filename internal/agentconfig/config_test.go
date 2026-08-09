package agentconfig_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qed-runtime/qed/agent"
	"github.com/qed-runtime/qed/extension/selfexec"
	workspaceextension "github.com/qed-runtime/qed/extensions/workspace"
	"github.com/qed-runtime/qed/internal/agentconfig"
	"github.com/qed-runtime/qed/internal/extensionregistry"
)

func TestMain(testingMain *testing.M) {
	if len(os.Args) >= 2 && os.Args[1] == selfexec.ChildArgument {
		handled, err := extensionregistry.Catalog.Dispatch(context.Background(), selfexec.DispatchOptions{
			Arguments: os.Args[1:],
			Input:     os.Stdin,
			Output:    os.Stdout,
		})
		if !handled || err != nil {
			_, _ = fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(testingMain.Run())
}

func TestLoadBuildsProviderProfilesAndAgentGraph(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, `{
		"version": 1,
		"default_agent": "coordinator",
		"limits": {
			"max_runs": 8,
			"max_depth": 3,
			"max_provider_calls": 24
		},
		"providers": {
			"primary": {"protocol": "echo"},
			"review": {"protocol": "echo"}
		},
		"agents": {
			"specialist": {
				"provider": "review",
				"instructions": "Review the task"
			},
			"coordinator": {
				"provider": "primary",
				"instructions": "Coordinate the answer",
				"delegations": [{
					"name": "consult_specialist",
					"description": "Ask the specialist",
					"strategy": "delegate",
					"agents": ["specialist"]
				}]
			}
		}
	}`)

	configuration, err := agentconfig.Load(path, agentconfig.LoadOptions{})
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	defer func() {
		if err := configuration.Close(); err != nil {
			t.Errorf("Configuration.Close() error = %v", err)
		}
	}()
	if got := strings.Join(configuration.Registry.AgentIDs(), ","); got != "coordinator,specialist" {
		t.Errorf("AgentIDs() = %q", got)
	}
	if got, err := configuration.ResolveAgent(""); err != nil || got != "coordinator" {
		t.Errorf("ResolveAgent(default) = %q, %v", got, err)
	}
	if got, err := configuration.ResolveAgent("specialist"); err != nil || got != "specialist" {
		t.Errorf("ResolveAgent(explicit) = %q, %v", got, err)
	}

	result, err := configuration.Registry.Run(context.Background(), agent.RunRequest{
		AgentID: "coordinator",
		Input:   []agent.Message{{Role: agent.RoleUser, Text: "hello"}},
	})
	if err != nil {
		t.Fatalf("Registry.Run() error = %v", err)
	}
	if result.AgentID != "coordinator" || result.Messages[len(result.Messages)-1].Text != "hello" {
		t.Errorf("Run result = %#v", result)
	}
}

func TestLoadCombinesManifestAndDiscoveredExtensions(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	explicit := writeManifestExtension(t, root, "explicit-extension")
	discoveryRoot := filepath.Join(root, "discovered")
	_ = writeManifestExtension(t, discoveryRoot, "discovered-extension")
	document := fmt.Sprintf(`{
		"version":1,
		"providers":{"local":{"protocol":"echo"}},
		"extensions":{"explicit-extension":{"mode":"manifest","manifest":%q}},
		"extension_directories":[%q],
		"extension_state":{"store":"memory"},
		"agents":{"main":{"provider":"local"}}
	}`, explicit, discoveryRoot)
	configuration, err := agentconfig.Load(writeConfig(t, document), agentconfig.LoadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer configuration.Close()
	if got := strings.Join(configuration.ExtensionIDs(), ","); got != "discovered-extension,explicit-extension" {
		t.Fatalf("ExtensionIDs() = %q", got)
	}
	if configuration.ExtensionStateStore == nil {
		t.Fatal("ExtensionStateStore is nil")
	}
	if err := configuration.ExtensionStateStore.Set(context.Background(), "explicit-extension", "test", "key", []byte("value")); err != nil {
		t.Fatal(err)
	}
	value, err := configuration.ExtensionStateStore.Get(context.Background(), "explicit-extension", "test", "key")
	if err != nil || string(value) != "value" {
		t.Fatalf("Extension state = %q, %v", value, err)
	}
}

func TestLoadRejectsInvalidConfiguration(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		document    string
		environment map[string]string
		want        string
	}{
		{
			name: "duplicate key",
			document: `{
				"version": 1,
				"version": 1,
				"providers": {"local": {"protocol": "echo"}},
				"agents": {"main": {"provider": "local"}}
			}`,
			want: `duplicate JSON key "version"`,
		},
		{
			name: "inline credential",
			document: `{
				"version": 1,
				"providers": {
					"primary": {
						"protocol": "openai-responses",
						"model": "test-model",
						"api_key": "must-not-be-supported"
					}
				},
				"agents": {"main": {"provider": "primary"}}
			}`,
			want: `unknown field "api_key"`,
		},
		{
			name: "missing credential environment",
			document: `{
				"version": 1,
				"providers": {
					"primary": {
						"protocol": "openai-responses",
						"model": "test-model",
						"token_env": "PRIMARY_TOKEN"
					}
				},
				"agents": {"main": {"provider": "primary"}}
			}`,
			want: `credential environment "PRIMARY_TOKEN" for profile "primary" is not set`,
		},
		{
			name: "missing ChatGPT auth profile",
			document: `{
				"version": 1,
				"providers": {
					"primary": {
						"protocol": "openai-codex",
						"model": "gpt-test"
					}
				},
				"agents": {"main": {"provider": "primary"}}
			}`,
			want: `auth_profile is required`,
		},
		{
			name: "custom Codex endpoint",
			document: `{
				"version": 1,
				"providers": {
					"primary": {
						"protocol": "openai-codex",
						"model": "gpt-test",
						"auth_profile": "personal",
						"base_url": "https://example.invalid"
					}
				},
				"agents": {"main": {"provider": "primary"}}
			}`,
			want: `accepts only protocol, model, and auth_profile`,
		},
		{
			name: "delegation cycle",
			document: `{
				"version": 1,
				"providers": {"local": {"protocol": "echo"}},
				"agents": {
					"alpha": {
						"provider": "local",
						"delegations": [{"name":"to_beta","strategy":"delegate","agents":["beta"]}]
					},
					"beta": {
						"provider": "local",
						"delegations": [{"name":"to_alpha","strategy":"delegate","agents":["alpha"]}]
					}
				}
			}`,
			want: "agent delegation cycle: alpha -> beta -> alpha",
		},
		{
			name: "unsupported version",
			document: `{
				"version": 2,
				"providers": {"local": {"protocol": "echo"}},
				"agents": {"main": {"provider": "local"}}
			}`,
			want: "unsupported configuration version 2, want 1",
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			path := writeConfig(t, test.document)
			_, err := agentconfig.Load(path, agentconfig.LoadOptions{LookupEnv: lookup(test.environment)})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Load() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestLoadBuildsOpenAICodexProfileFromNamedAuth(t *testing.T) {
	t.Parallel()

	authPath := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(authPath, []byte(`{
		"version": 1,
		"profiles": {
			"personal": {
				"type": "chatgpt",
				"id_token": "stored-id-token",
				"access_token": "stored-access-token",
				"refresh_token": "stored-refresh-token",
				"account_id": "account-1",
				"expires_at": "2099-01-01T00:00:00Z",
				"updated_at": "2026-08-09T00:00:00Z"
			}
		}
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	path := writeConfig(t, `{
		"version": 1,
		"providers": {
			"codex": {
				"protocol": "openai-codex",
				"model": "gpt-test",
				"auth_profile": "personal"
			}
		},
		"agents": {"main": {"provider": "codex"}}
	}`)
	configuration, err := agentconfig.Load(path, agentconfig.LoadOptions{AuthStorePath: authPath})
	if err != nil {
		t.Fatal(err)
	}
	if err := configuration.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestLoadAllowsUnauthenticatedCustomEndpoint(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, `{
		"version": 1,
		"providers": {
			"local": {
				"protocol": "openai-chat",
				"base_url": "http://127.0.0.1:8080/v1",
				"model": "local-model"
			}
		},
		"agents": {"main": {"provider": "local"}}
	}`)
	configuration, err := agentconfig.Load(path, agentconfig.LoadOptions{})
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got, err := configuration.ResolveAgent("main"); err != nil || got != "main" {
		t.Errorf("ResolveAgent() = %q, %v", got, err)
	}
	if _, err := configuration.ResolveAgent(""); err == nil || !strings.Contains(err.Error(), "default_agent") {
		t.Errorf("ResolveAgent(default) error = %v", err)
	}
}

func TestLoadBuildsWorkspaceBoundCodingProfile(t *testing.T) {
	t.Parallel()

	workspaceRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspaceRoot, "AGENTS.md"), []byte("# Instructions\n\nVerify changes\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := writeConfig(t, `{
		"version": 1,
		"default_agent": "main",
		"providers": {"local": {"protocol": "echo"}},
		"extensions": {
			"qed.workspace": {"mode": "self-exec"},
			"qed.process": {"mode": "self-exec"},
			"qed.git": {"mode": "self-exec"}
		},
		"profiles": {
			"workspace": {
				"kind": "coding",
				"extensions": ["qed.workspace", "qed.process", "qed.git"],
				"capabilities": {
					"allow": ["filesystem.read", "filesystem.write", "process.execute", "git.read"],
					"deny": ["filesystem.delete"]
				},
				"environment": ["PATH"]
			}
		},
		"agents": {
			"main": {"provider": "local", "profile": "workspace"}
		}
	}`)
	configuration, err := agentconfig.Load(path, agentconfig.LoadOptions{
		LookupEnv:       lookup(map[string]string{"PATH": "/test/bin"}),
		WorkspaceRoot:   workspaceRoot,
		SelfExecutable:  testExecutable(t),
		SelfExecCatalog: extensionregistry.Catalog,
	})
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	defer func() {
		if err := configuration.Close(); err != nil {
			t.Errorf("Configuration.Close() error = %v", err)
		}
	}()
	if configuration.Recorder() == nil || configuration.ToolInvocations() == nil {
		t.Fatal("Load() did not create an in-memory Evidence recorder")
	}
	if got := strings.Join(configuration.ExtensionIDs(), ","); got != "qed.git,qed.process,qed.workspace" {
		t.Fatalf("ExtensionIDs() = %q", got)
	}
	result, err := configuration.Registry.Run(context.Background(), agent.RunRequest{
		AgentID: "main",
		Input:   []agent.Message{{Role: agent.RoleUser, Text: "hello"}},
	})
	if err != nil {
		t.Fatalf("Registry.Run() error = %v", err)
	}
	if result.Messages[len(result.Messages)-1].Text != "hello" {
		t.Fatalf("Run result = %#v", result)
	}
}

func TestLoadBuildsExternalWorkspaceExtension(t *testing.T) {
	t.Parallel()

	workspaceRoot := t.TempDir()
	executable := testExecutable(t)
	document := fmt.Sprintf(`{
		"version":1,
		"default_agent":"main",
		"providers":{"local":{"protocol":"echo"}},
		"extensions":{"qed.workspace":{"mode":"external","command":[%q,%q,%q]}},
		"profiles":{"workspace":{
			"kind":"coding",
			"extensions":["qed.workspace"],
			"capabilities":{"allow":["filesystem.read"]}
		}},
		"agents":{"main":{"provider":"local","profile":"workspace"}}
	}`, executable, selfexec.ChildArgument, workspaceextension.ID)
	path := writeConfig(t, document)
	configuration, err := agentconfig.Load(path, agentconfig.LoadOptions{WorkspaceRoot: workspaceRoot})
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	defer func() {
		if err := configuration.Close(); err != nil {
			t.Errorf("Configuration.Close() error = %v", err)
		}
	}()
	result, err := configuration.Registry.Run(context.Background(), agent.RunRequest{
		AgentID: "main",
		Input:   []agent.Message{{Role: agent.RoleUser, Text: "external"}},
	})
	if err != nil || result.Messages[len(result.Messages)-1].Text != "external" {
		t.Fatalf("Registry.Run() = %#v, %v", result, err)
	}
}

func TestLoadRejectsInvalidCodingProfile(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		profile   string
		agent     string
		workspace string
		want      string
	}{
		{
			name:      "workspace required",
			profile:   `{"kind":"coding","extensions":["qed.workspace"],"capabilities":{"allow":["filesystem.read"]}}`,
			agent:     `{"provider":"local","profile":"coding"}`,
			workspace: "",
			want:      "workspace root is required",
		},
		{
			name:      "capabilities required",
			profile:   `{"kind":"coding","extensions":["qed.workspace"]}`,
			agent:     `{"provider":"local","profile":"coding"}`,
			workspace: t.TempDir(),
			want:      "capabilities are required",
		},
		{
			name:      "invalid capability",
			profile:   `{"kind":"coding","extensions":["qed.workspace"],"capabilities":{"allow":["network unrestricted"]}}`,
			agent:     `{"provider":"local","profile":"coding"}`,
			workspace: t.TempDir(),
			want:      "unsupported character",
		},
		{
			name:      "unknown reference",
			profile:   `{"kind":"coding","extensions":["qed.workspace"],"capabilities":{"allow":["filesystem.read"]}}`,
			agent:     `{"provider":"local","profile":"missing"}`,
			workspace: t.TempDir(),
			want:      `references unknown Profile "missing"`,
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			document := fmt.Sprintf(`{
					"version":1,
					"providers":{"local":{"protocol":"echo"}},
					"extensions":{"qed.workspace":{"mode":"self-exec"}},
					"profiles":{"coding":%s},
				"agents":{"main":%s}
			}`, test.profile, test.agent)
			path := writeConfig(t, document)
			_, err := agentconfig.Load(path, agentconfig.LoadOptions{
				WorkspaceRoot:   test.workspace,
				SelfExecutable:  testExecutable(t),
				SelfExecCatalog: extensionregistry.Catalog,
			})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Load() error = %v, want %q", err, test.want)
			}
		})
	}
}

func testExecutable(t *testing.T) string {
	t.Helper()
	path, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	path, err = filepath.Abs(path)
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func writeManifestExtension(t *testing.T, root, extensionID string) string {
	t.Helper()
	directory := filepath.Join(root, extensionID)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "extension"), []byte("test executable"), 0o700); err != nil {
		t.Fatal(err)
	}
	document := fmt.Sprintf(`{
		"id":%q,
		"version":"v1",
		"protocol_version":1,
		"entrypoint":"extension"
	}`, extensionID)
	if err := os.WriteFile(filepath.Join(directory, "qed-extension.json"), []byte(document), 0o600); err != nil {
		t.Fatal(err)
	}
	return directory
}

func writeConfig(t *testing.T, document string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "qed.json")
	if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
		t.Fatalf("write configuration: %v", err)
	}
	return path
}

func lookup(values map[string]string) agentconfig.LookupEnv {
	return func(name string) (string, bool) {
		value, ok := values[name]
		return value, ok
	}
}
