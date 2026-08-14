package agentconfig_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/qed-runtime/qed/agent"
	"github.com/qed-runtime/qed/extension/selfexec"
	workspaceextension "github.com/qed-runtime/qed/extensions/workspace"
	"github.com/qed-runtime/qed/internal/agentconfig"
	"github.com/qed-runtime/qed/internal/extensionregistry"
	"github.com/qed-runtime/qed/orchestration"
	"github.com/qed-runtime/qed/profile/coding"
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
			"primary": {
				"protocol": "echo",
				"rate_limit": {"max_concurrency": 2}
			},
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
				"provider_retry": {
					"max_attempts": 2,
					"initial_backoff": "1ms",
					"max_backoff": "2ms"
				},
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

func TestLoadEnablesContextRetrievalTools(t *testing.T) {
	t.Parallel()

	validator := &countingToolInputValidator{}
	semanticScorer := &configContextSemanticScorer{}
	tokenEstimator := &configTokenEstimator{}
	path := writeConfig(t, `{
		"version": 1,
		"providers": {"local": {"protocol": "echo"}},
		"evidence": {"store": "json", "path": "evidence"},
		"agents": {"main": {
			"provider": "local",
			"context": {
				"max_input_bytes": 4096,
				"checkpoint_max_bytes": 512,
				"retrieval": {
					"max_calls_per_run": 2,
					"max_items_per_call": 2,
					"max_items_per_run": 4,
					"max_output_bytes_per_call": 1024,
					"max_output_bytes_per_run": 2048
				}
			}
		}}
	}`)
	configuration, err := agentconfig.Load(path, agentconfig.LoadOptions{
		ToolInputValidator: validator, ContextSemanticScorer: semanticScorer,
		TokenEstimator: tokenEstimator,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer configuration.Close()
	if validator.Count() != 5 {
		t.Fatalf("compiled Tool schemas = %d, want 5 Context retrieval Tools", validator.Count())
	}
	result, err := configuration.Registry.Run(context.Background(), agent.RunRequest{
		AgentID: "main", Input: []agent.Message{{Role: agent.RoleUser, Text: "hello"}},
	})
	if err != nil || result.Status != agent.RunStatusCompleted {
		t.Fatalf("configured Run = %#v, %v", result, err)
	}
	if semanticScorer.Calls() != 0 {
		t.Fatalf("Echo Run unexpectedly invoked semantic scorer %d times", semanticScorer.Calls())
	}
	if tokenEstimator.Calls() == 0 {
		t.Fatal("Echo Run did not invoke configured Token Estimator")
	}
}

func TestLoadSharesRateLimiterAcrossAgentsUsingOneProviderProfile(t *testing.T) {
	t.Parallel()

	providerStarted := make(chan struct{}, 2)
	releaseProvider := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/responses" {
			t.Errorf("request path = %q, want /responses", request.URL.Path)
		}
		providerStarted <- struct{}{}
		select {
		case <-request.Context().Done():
			return
		case <-releaseProvider:
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(writer, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"rate-test\",\"model\":\"test-model\",\"status\":\"completed\",\"output\":[]}}\n\n")
	}))
	defer server.Close()
	defer close(releaseProvider)

	path := writeConfig(t, fmt.Sprintf(`{
		"version": 1,
		"providers": {
			"shared": {
				"protocol": "openai-responses",
				"base_url": %q,
				"model": "test-model",
				"token_env": "TEST_TOKEN",
				"rate_limit": {"max_concurrency": 1}
			}
		},
		"agents": {
			"first": {"provider": "shared"},
			"second": {"provider": "shared"}
		}
	}`, server.URL))
	configuration, err := agentconfig.Load(path, agentconfig.LoadOptions{
		LookupEnv: func(name string) (string, bool) {
			return "test-token", name == "TEST_TOKEN"
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer configuration.Close()

	type runOutcome struct {
		events []agent.Event
		result agent.RunResult
		err    error
	}
	start := func(agentID string) (<-chan runOutcome, <-chan struct{}) {
		handle, startErr := configuration.Registry.Start(context.Background(), agent.RunRequest{
			AgentID: agentID,
			Input:   []agent.Message{{Role: agent.RoleUser, Text: "start"}},
		})
		if startErr != nil {
			t.Fatal(startErr)
		}
		outcome := make(chan runOutcome, 1)
		waiting := make(chan struct{}, 1)
		go func() {
			var events []agent.Event
			for event := range handle.Events() {
				events = append(events, event)
				if event.Type == agent.EventProviderRateLimitWait {
					select {
					case waiting <- struct{}{}:
					default:
					}
				}
			}
			result, runErr := handle.Wait()
			outcome <- runOutcome{events: events, result: result, err: runErr}
		}()
		return outcome, waiting
	}

	first, _ := start("first")
	select {
	case <-providerStarted:
	case <-time.After(time.Second):
		t.Fatal("first Provider request did not start")
	}
	second, secondWaiting := start("second")
	select {
	case <-secondWaiting:
	case <-providerStarted:
		t.Fatal("second Provider request reached the server before waiting")
	case <-time.After(time.Second):
		t.Fatal("second Agent did not report Provider capacity wait")
	}

	releaseProvider <- struct{}{}
	select {
	case <-providerStarted:
	case <-time.After(time.Second):
		t.Fatal("queued Provider request did not start")
	}
	releaseProvider <- struct{}{}

	for index, outcomeChannel := range []<-chan runOutcome{first, second} {
		outcome := <-outcomeChannel
		if outcome.err != nil || outcome.result.Status != agent.RunStatusCompleted {
			t.Fatalf("Run %d = %#v, %v", index+1, outcome.result, outcome.err)
		}
		if outcome.result.ProviderCalls != 1 {
			t.Fatalf("Run %d Provider calls = %d, want 1", index+1, outcome.result.ProviderCalls)
		}
		if index == 1 {
			var wait *agent.ProviderRateLimitWaitInfo
			for _, event := range outcome.events {
				if event.Type == agent.EventProviderRateLimitWait {
					wait = event.ProviderRateLimitWait
					break
				}
			}
			if wait == nil || wait.Reason != agent.ProviderRateLimitWaitConcurrency || wait.MaxConcurrency != 1 {
				t.Fatalf("second Run Provider wait = %#v", wait)
			}
		}
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
			want: `accepts only protocol, model, auth_profile, pricing, and rate_limit`,
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
		{
			name: "invalid Provider retry duration",
			document: `{
				"version": 1,
				"providers": {"local": {"protocol": "echo"}},
				"agents": {"main": {
					"provider": "local",
					"provider_retry": {"initial_backoff": "soon"}
				}}
			}`,
			want: `initial_backoff "soon" must be a positive Go duration`,
		},
		{
			name: "Provider retry maximum below initial",
			document: `{
				"version": 1,
				"providers": {"local": {"protocol": "echo"}},
				"agents": {"main": {
					"provider": "local",
					"provider_retry": {
						"initial_backoff": "2s",
						"max_backoff": "1s"
					}
				}}
			}`,
			want: "Provider retry max backoff must not be shorter than initial backoff",
		},
		{
			name: "negative Provider concurrency",
			document: `{
				"version": 1,
				"providers": {
					"local": {
						"protocol": "echo",
						"rate_limit": {"max_concurrency": -1}
					}
				},
				"agents": {"main": {"provider": "local"}}
			}`,
			want: "Provider rate limit max concurrency must be between 1 and 1024 when set",
		},
		{
			name: "context without evidence store",
			document: `{
				"version": 1,
				"providers": {"local": {"protocol": "echo"}},
				"agents": {"main": {"provider": "local", "context": {"max_input_bytes": 4096}}}
			}`,
			want: "context compaction requires a JSON Evidence Store",
		},
		{
			name: "context Rebase interval too large",
			document: `{
				"version": 1,
				"providers": {"local": {"protocol": "echo"}},
				"evidence": {"store": "json", "path": "evidence"},
				"agents": {"main": {
					"provider": "local",
					"context": {
						"max_input_bytes": 4096,
						"checkpoint_max_bytes": 512,
						"rebase_generation_interval": 65
					}
				}}
			}`,
			want: "Context Rebase generation interval exceeds 64",
		},
		{
			name: "invalid Context retrieval call limit",
			document: `{
				"version": 1,
				"providers": {"local": {"protocol": "echo"}},
				"evidence": {"store": "json", "path": "evidence"},
				"agents": {"main": {
					"provider": "local",
					"context": {
						"max_input_bytes": 4096,
						"checkpoint_max_bytes": 512,
						"retrieval": {"max_calls_per_run": -1}
					}
				}}
			}`,
			want: "Context retrieval max calls per Run",
		},
		{
			name: "Predictive Budget reserves all input",
			document: `{
				"version": 1,
				"providers": {"local": {"protocol": "echo"}},
				"evidence": {"store": "json", "path": "evidence"},
				"agents": {"main": {
					"provider": "local",
					"context": {
						"max_input_bytes": 4096,
						"checkpoint_max_bytes": 512,
						"predictive_budget": {
							"context_window_tokens": 100,
							"output_reserve_tokens": 80,
							"predicted_tool_output_tokens": 20,
							"soft_threshold_tokens": 90
						}
					}
				}}
			}`,
			want: "Predictive Budget reserves leave no model input capacity",
		},
		{
			name: "Evidence isolation key with surrounding whitespace",
			document: `{
				"version": 1,
				"providers": {"local": {"protocol": "echo"}},
				"evidence": {"store": "json", "path": "evidence", "isolation_key": "tenant "},
				"agents": {"main": {"provider": "local"}}
			}`,
			want: "tenant ID is required and must not have surrounding whitespace",
		},
		{
			name: "unsupported Evidence sensitivity",
			document: `{
				"version": 1,
				"providers": {"local": {"protocol": "echo"}},
				"evidence": {"store": "json", "path": "evidence"},
				"agents": {"main": {
					"provider": "local",
					"context": {
						"max_input_bytes": 4096,
						"checkpoint_max_bytes": 512,
						"evidence_sensitivity": "classified"
					}
				}}
			}`,
			want: "unsupported Evidence sensitivity",
		},
		{
			name: "incomplete cache pricing",
			document: `{
				"version": 1,
				"providers": {"local": {
					"protocol": "echo",
					"pricing": {
						"currency": "USD",
						"uncached_input_micros_per_million": 1,
						"cache_read_micros_per_million": 0,
						"cache_write_micros_per_million": 1
					}
				}},
				"agents": {"main": {"provider": "local"}}
			}`,
			want: "cache read and write prices are required",
		},
		{
			name: "invalid cache capability override",
			document: `{
				"version": 1,
				"providers": {"local": {
					"protocol": "openai-responses",
					"base_url": "https://example.invalid/v1",
					"model": "custom-model",
					"cache_capabilities": {
						"supports_explicit": true,
						"max_write_breakpoints": 0
					}
				}},
				"agents": {"main": {"provider": "local"}}
			}`,
			want: "explicit Cache Capability requires at least one breakpoint",
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

func TestLoadBuildsContextAndCacheConfiguration(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, `{
		"version": 1,
		"providers": {
			"local": {
				"protocol": "echo",
				"pricing": {
					"currency": "USD",
					"uncached_input_micros_per_million": 2000000,
					"cache_read_micros_per_million": 200000,
					"cache_write_micros_per_million": 2500000,
					"output_micros_per_million": 10000000
				}
			}
		},
		"evidence": {"store": "json", "path": "evidence"},
		"agents": {
			"main": {
				"provider": "local",
				"context": {
					"max_input_bytes": 4700,
					"recent_messages": 2,
					"checkpoint_max_bytes": 3200,
					"predictive_budget": {
						"context_window_tokens": 100000,
						"output_reserve_tokens": 1000,
						"safety_margin_tokens": 500,
						"predicted_tool_output_tokens": 1000,
						"soft_threshold_tokens": 90000
					}
				},
				"cache": {
					"mode": "adaptive",
					"expected_reuse": 3,
					"isolation_key": "tenant-a",
					"family": "project-a"
				}
			}
		}
	}`)
	configuration, err := agentconfig.Load(path, agentconfig.LoadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer configuration.Close()
	inputs := make([]agent.Message, 10)
	for index := range inputs {
		inputs[index] = agent.Message{Role: agent.RoleUser, Text: strings.Repeat("x", 500)}
	}
	result, err := configuration.Registry.Run(context.Background(), agent.RunRequest{
		AgentID: "main",
		Input:   inputs,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.ContextCheckpoint == nil || result.CachePlan == nil || result.CachePlan.Mode != agent.CacheModeDisabled ||
		result.PredictiveBudget == nil || result.PredictiveBudget.Level != agent.PredictiveBudgetWithin {
		t.Fatalf("configured Run result = %#v", result)
	}
	objects, ok := configuration.EvidenceStore.(agent.EvidenceObjectAdminStore)
	if !ok || len(result.ContextCheckpoint.Evidence) == 0 || result.ContextCheckpoint.Evidence[0].Scope == nil {
		t.Fatal("configuration did not expose Checkpoint Evidence Objects")
	}
	if _, err := objects.GetObjectAdmin(
		context.Background(), result.ContextCheckpoint.Evidence[0], "agentconfig-test",
	); err != nil {
		t.Fatalf("load Checkpoint source Evidence: %v", err)
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
	if result.CurrentWorldState == nil || !result.CurrentWorldState.Snapshot.FilesAvailable {
		t.Fatalf("declarative Coding Profile Current World State = %#v", result.CurrentWorldState)
	}
	foundInstructions := false
	for _, file := range result.CurrentWorldState.Snapshot.Files {
		if file.Path == "AGENTS.md" && file.Status == agent.CurrentWorldFilePresent {
			foundInstructions = true
			break
		}
	}
	if !foundInstructions {
		t.Fatalf("Current World State files = %#v", result.CurrentWorldState.Snapshot.Files)
	}
	team, err := configuration.Registry.RunTeam(context.Background(), orchestration.TeamRequest{
		Strategy: orchestration.TeamStrategyDelegate,
		AgentIDs: []string{"main"},
		Input:    []agent.Message{{Role: agent.RoleUser, Text: "project state"}},
	})
	if err != nil {
		t.Fatalf("Coding Profile RunTeam() error = %v", err)
	}
	if len(team.Candidates) != 1 || team.Candidates[0].ResultPacket == nil ||
		len(team.Candidates[0].ResultPacket.ProfileState) == 0 {
		t.Fatalf("declarative Coding Profile Result Packet = %#v", team.Candidates)
	}
	var resultState coding.ResultState
	if err := json.Unmarshal(team.Candidates[0].ResultPacket.ProfileState, &resultState); err != nil ||
		resultState.Version != coding.ResultStateVersion || resultState.CurrentWorldState == nil {
		t.Fatalf("declarative Coding Profile ResultState = %#v, %v", resultState, err)
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

type countingToolInputValidator struct {
	mu    sync.Mutex
	count int
}

func (validator *countingToolInputValidator) Compile(
	schema json.RawMessage,
) (agent.CompiledToolInputValidator, error) {
	compiled, err := (agent.JSONSchemaSubsetValidator{}).Compile(schema)
	if err != nil {
		return nil, err
	}
	validator.mu.Lock()
	validator.count++
	validator.mu.Unlock()
	return compiled, nil
}

func (validator *countingToolInputValidator) Count() int {
	validator.mu.Lock()
	defer validator.mu.Unlock()
	return validator.count
}

type configContextSemanticScorer struct {
	mu    sync.Mutex
	calls int
}

func (scorer *configContextSemanticScorer) Score(
	context.Context,
	agent.ContextSemanticScoreRequest,
) ([]int, error) {
	scorer.mu.Lock()
	scorer.calls++
	scorer.mu.Unlock()
	return nil, nil
}

func (scorer *configContextSemanticScorer) Calls() int {
	scorer.mu.Lock()
	defer scorer.mu.Unlock()
	return scorer.calls
}

var _ agent.ContextSemanticScorer = (*configContextSemanticScorer)(nil)

type configTokenEstimator struct {
	mu    sync.Mutex
	calls int
}

func (estimator *configTokenEstimator) EstimateTokens(
	_ context.Context,
	request agent.TokenEstimateRequest,
) (agent.TokenEstimateResult, error) {
	estimator.mu.Lock()
	estimator.calls++
	estimator.mu.Unlock()
	return agent.TokenEstimateResult{
		Kind: "config_tokenizer", Tokens: make([]int64, len(request.Items)),
	}, nil
}

func (estimator *configTokenEstimator) Calls() int {
	estimator.mu.Lock()
	defer estimator.mu.Unlock()
	return estimator.calls
}

var _ agent.TokenEstimator = (*configTokenEstimator)(nil)
