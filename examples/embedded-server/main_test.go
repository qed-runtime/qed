package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/qed-runtime/qed"
	"github.com/qed-runtime/qed/examples/embedded-server/extensionregistry"
	"github.com/qed-runtime/qed/extension/selfexec"
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

func TestHandlerRunsLoadedHostWithSelfExecExtension(t *testing.T) {
	t.Parallel()

	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve example directory")
	}
	directory := filepath.Dir(sourceFile)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	host, err := qed.LoadHost(filepath.Join(directory, "qed.json"), qed.HostLoadOptions{
		WorkspaceRoot:   t.TempDir(),
		SelfExecutable:  executable,
		SelfExecCatalog: extensionregistry.Catalog,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := host.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	}()
	if got := host.ExtensionIDs(); len(got) != 1 || got[0] != "example.greeting" {
		t.Fatalf("ExtensionIDs() = %v", got)
	}

	request := httptest.NewRequest(http.MethodPost, "/runs", bytes.NewBufferString(`{"prompt":"hello"}`))
	response := httptest.NewRecorder()
	newHandler(host).ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("response = %d %s", response.Code, response.Body.String())
	}
	var document struct {
		Result struct {
			Status string `json:"status"`
		} `json:"result"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &document); err != nil {
		t.Fatal(err)
	}
	if document.Result.Status != "completed" {
		t.Fatalf("Run status = %q, body = %s", document.Result.Status, response.Body.String())
	}
}
