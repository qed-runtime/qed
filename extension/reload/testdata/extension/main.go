package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/qed-runtime/qed/agent"
	"github.com/qed-runtime/qed/extension/protocol"
	"github.com/qed-runtime/qed/extension/server"
)

func main() {
	err := server.Serve(context.Background(), os.Stdin, os.Stdout, server.Options{
		ID:      "reload-development-test",
		Version: "v1",
		Initialize: func(context.Context, protocol.InitializeRequest) ([]agent.Tool, error) {
			return nil, nil
		},
		Snapshot: func(context.Context) (json.RawMessage, error) {
			return json.RawMessage(`{"generation_state":true}`), nil
		},
		Restore: func(context.Context, json.RawMessage) error {
			return nil
		},
	})
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
