package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/qed-runtime/qed"
	"github.com/qed-runtime/qed/agent"
	"github.com/qed-runtime/qed/examples/embedded-server/extensionregistry"
	"github.com/qed-runtime/qed/extension/selfexec"
)

const maximumRequestBytes = 1 << 20

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		log.New(os.Stderr, "embedded-server: ", 0).Print(err)
		os.Exit(1)
	}
}

func run(ctx context.Context, arguments []string, stdin io.Reader, stdout, stderr io.Writer) (runErr error) {
	handled, err := extensionregistry.Catalog.Dispatch(ctx, selfexec.DispatchOptions{
		Arguments:   arguments,
		Input:       stdin,
		Output:      stdout,
		DebugWriter: stderr,
	})
	if handled {
		return err
	}
	if ctx == nil {
		return errors.New("server context must not be nil")
	}

	flags := flag.NewFlagSet("embedded-server", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configurationPath := flags.String("config", "qed.json", "QED Agent configuration")
	workspaceRoot := flags.String("workspace", ".", "workspace root")
	listenAddress := flags.String("listen", "127.0.0.1:8080", "HTTP listen address")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("embedded-server does not accept positional arguments")
	}
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve current executable: %w", err)
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		return fmt.Errorf("resolve absolute current executable: %w", err)
	}
	host, err := qed.LoadHost(*configurationPath, qed.HostLoadOptions{
		LookupEnv:       os.LookupEnv,
		WorkspaceRoot:   *workspaceRoot,
		SelfExecutable:  executable,
		SelfExecCatalog: extensionregistry.Catalog,
	})
	if err != nil {
		return fmt.Errorf("load QED Host: %w", err)
	}
	defer func() {
		runErr = errors.Join(runErr, host.Close())
	}()

	serverContext, cancelServer := context.WithCancel(ctx)
	defer cancelServer()
	httpServer := &http.Server{
		Addr:              *listenAddress,
		Handler:           newHandler(host),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       30 * time.Second,
		BaseContext: func(net.Listener) context.Context {
			return serverContext
		},
	}
	shutdownResult := make(chan error, 1)
	go func() {
		<-serverContext.Done()
		shutdownContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		shutdownResult <- httpServer.Shutdown(shutdownContext)
	}()
	err = httpServer.ListenAndServe()
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		cancelServer()
		<-shutdownResult
		return fmt.Errorf("serve HTTP: %w", err)
	}
	if err := <-shutdownResult; err != nil {
		return fmt.Errorf("shut down HTTP server: %w", err)
	}
	return nil
}

func newHandler(host *qed.Host) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{"status":"ok"}`+"\n")
	})
	mux.HandleFunc("POST /runs", func(writer http.ResponseWriter, request *http.Request) {
		request.Body = http.MaxBytesReader(writer, request.Body, maximumRequestBytes)
		defer request.Body.Close()
		var input struct {
			AgentID   string `json:"agent_id,omitempty"`
			SessionID string `json:"session_id,omitempty"`
			Prompt    string `json:"prompt"`
		}
		decoder := json.NewDecoder(request.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&input); err != nil {
			writeError(writer, http.StatusBadRequest, "invalid request")
			return
		}
		var trailing any
		if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
			writeError(writer, http.StatusBadRequest, "invalid request")
			return
		}
		if input.Prompt == "" {
			writeError(writer, http.StatusBadRequest, "prompt is required")
			return
		}
		outcome, err := host.Run(request.Context(), agent.RunRequest{
			AgentID:   input.AgentID,
			SessionID: input.SessionID,
			Input:     []agent.Message{{Role: agent.RoleUser, Text: input.Prompt}},
		}, nil)
		if err != nil {
			writeError(writer, http.StatusInternalServerError, "Agent Run failed")
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(writer).Encode(outcome); err != nil {
			return
		}
	})
	return mux
}

func writeError(writer http.ResponseWriter, status int, message string) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(map[string]string{"error": message})
}
