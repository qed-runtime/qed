package reload_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/qed-runtime/qed/capability"
	"github.com/qed-runtime/qed/extension/reload"
)

func TestDeveloperBuildsFromSourceAndReloadsGeneration(t *testing.T) {
	policy, err := capability.NewStaticPolicy(capability.StaticPolicyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	controlDirectory := filepath.Join(t.TempDir(), "control")
	var status bytes.Buffer
	developer, err := reload.StartDev(context.Background(), reload.DevOptions{
		ManifestPath:     filepath.Join("testdata", "extension"),
		Policy:           policy,
		ControlDirectory: controlDirectory,
		StatusWriter:     &status,
	})
	if err != nil {
		t.Fatal(err)
	}
	first, err := developer.Status(context.Background())
	if err != nil || first.Generation != 1 || first.ExtensionID != "reload-development-test" {
		t.Fatalf("initial Status = %#v, %v", first, err)
	}
	second, err := developer.Reload(context.Background())
	if err != nil || second.Generation != 2 {
		t.Fatalf("Reload = %#v, %v", second, err)
	}
	if err := developer.Close(); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(controlDirectory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("control descriptors after Close = %d", len(entries))
	}
	if !strings.Contains(status.String(), `started Extension "reload-development-test" generation 1`) {
		t.Fatalf("status output = %q", status.String())
	}
}

func TestControlServerAuthenticatesStatusAndReload(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	directory := filepath.Join(t.TempDir(), "control")
	handler := &controlHandler{generation: 1}
	server, err := reload.StartControl(ctx, directory, "test-extension", handler)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := server.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	}()
	if duplicate, err := reload.StartControl(ctx, directory, "test-extension", handler); err == nil {
		_ = duplicate.Close()
		t.Fatal("second StartControl() succeeded for an active Extension")
	}
	status, err := reload.RequestControl(context.Background(), directory, "test-extension", "status")
	if err != nil || status.Generation != 1 {
		t.Fatalf("status = %#v, %v", status, err)
	}
	status, err = reload.RequestControl(context.Background(), directory, "test-extension", "reload")
	if err != nil || status.Generation != 2 {
		t.Fatalf("reload = %#v, %v", status, err)
	}
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) != 1 {
		t.Fatalf("control descriptors = %d, %v", len(entries), err)
	}
	info, err := entries[0].Info()
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("control permissions = %v", info.Mode().Perm())
	}
}

func TestControlServerCloseAfterContextCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	directory := filepath.Join(t.TempDir(), "control")
	server, err := reload.StartControl(ctx, directory, "cancel-extension", &controlHandler{})
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := server.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("control descriptors after Close = %d", len(entries))
	}
}

type controlHandler struct {
	mu         sync.Mutex
	generation uint64
}

func (handler *controlHandler) Reload(context.Context) (reload.ControlStatus, error) {
	handler.mu.Lock()
	defer handler.mu.Unlock()
	handler.generation++
	return reload.ControlStatus{ExtensionID: "test-extension", Version: "v1", Generation: handler.generation}, nil
}

func (handler *controlHandler) Status(context.Context) (reload.ControlStatus, error) {
	handler.mu.Lock()
	defer handler.mu.Unlock()
	return reload.ControlStatus{ExtensionID: "test-extension", Version: "v1", Generation: handler.generation}, nil
}

func TestWatchDetectsSourceChange(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	path := filepath.Join(root, "source.go")
	if err := os.WriteFile(path, []byte("package source\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	detected := errors.New("change detected")
	result := make(chan error, 1)
	go func() {
		result <- reload.Watch(ctx, reload.WatchOptions{Root: root}, func(context.Context) error {
			return detected
		})
	}()
	time.Sleep(600 * time.Millisecond)
	if err := os.WriteFile(path, []byte("package source\n// changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if !errors.Is(err, detected) {
			t.Fatalf("Watch() error = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Watch() did not detect the source change")
	}
}
