package qed_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"github.com/qed-runtime/qed"
	"github.com/qed-runtime/qed/agent"
	"github.com/qed-runtime/qed/orchestration"
	"github.com/qed-runtime/qed/provider/echo"
)

func TestLoadHostRunsDefaultAgentAndSavesEvidence(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()
	configurationPath := filepath.Join(directory, "qed.json")
	if err := os.WriteFile(configurationPath, []byte(`{
		"version":1,
		"default_agent":"main",
		"providers":{"local":{"protocol":"echo"}},
		"agents":{"main":{"provider":"local"}},
		"evidence":{"store":"json","path":"evidence"}
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	host, err := qed.LoadHost(configurationPath, qed.HostLoadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := host.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	}()
	if host.DefaultAgent() != "main" || !reflect.DeepEqual(host.AgentIDs(), []string{"main"}) {
		t.Fatalf("Host agents = %q/%v", host.DefaultAgent(), host.AgentIDs())
	}
	var observed []agent.EventType
	outcome, err := host.Run(context.Background(), agent.RunRequest{
		Input: []agent.Message{{Role: agent.RoleUser, Text: "hello"}},
	}, func(_ context.Context, _ *agent.RunHandle, event agent.Event) error {
		observed = append(observed, event.Type)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Result.Status != agent.RunStatusCompleted || outcome.Evidence == nil {
		t.Fatalf("Run() = %#v", outcome)
	}
	if len(observed) == 0 || len(outcome.Events) != len(observed) {
		t.Fatalf("observed Events = %v, outcome Events = %d", observed, len(outcome.Events))
	}
	loaded, err := host.EvidenceStore().Load(context.Background(), outcome.Result.RunID)
	if err != nil || loaded.Run.ID != outcome.Result.RunID {
		t.Fatalf("Evidence Load() = %#v, %v", loaded, err)
	}
}

func TestProgrammaticHostResolvesAgentsWithoutOwningEvidence(t *testing.T) {
	t.Parallel()

	runtime, err := agent.NewRuntime(agent.Options{Provider: echo.New()})
	if err != nil {
		t.Fatal(err)
	}
	registry, err := orchestration.NewAgentRegistry(orchestration.AgentRegistryOptions{
		Agents: []orchestration.AgentDefinition{{ID: "main", Runtime: runtime}},
	})
	if err != nil {
		t.Fatal(err)
	}
	host, err := qed.NewHost(registry, "main")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := host.SaveRunEvidence(context.Background(), agent.RunResult{}, nil); !errors.Is(err, qed.ErrEvidenceUnavailable) {
		t.Fatalf("SaveRunEvidence() error = %v", err)
	}
	if _, err := qed.NewHost(registry, "missing"); err == nil {
		t.Fatal("NewHost() accepted an unregistered default Agent")
	}

	var waitGroup sync.WaitGroup
	errorsByRun := make(chan error, 4)
	for index := 0; index < 4; index++ {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			outcome, err := host.Run(context.Background(), agent.RunRequest{
				Input: []agent.Message{{Role: agent.RoleUser, Text: "concurrent"}},
			}, nil)
			if err == nil && outcome.Result.Status != agent.RunStatusCompleted {
				err = errors.New("concurrent Run did not complete")
			}
			errorsByRun <- err
		}()
	}
	waitGroup.Wait()
	close(errorsByRun)
	for err := range errorsByRun {
		if err != nil {
			t.Fatal(err)
		}
	}
}
