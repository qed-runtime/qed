package reload

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/qed-runtime/qed/internal/jsonstrict"
)

const maximumControlBytes = 64 << 10

// ControlStatus describes one running Extension development host
type ControlStatus struct {
	ExtensionID string `json:"extension_id"`
	Version     string `json:"version"`
	Generation  uint64 `json:"generation"`
	Manifest    string `json:"manifest"`
}

// ControlHandler handles authenticated requests from reload and inspect clients
type ControlHandler interface {
	Reload(ctx context.Context) (ControlStatus, error)
	Status(ctx context.Context) (ControlStatus, error)
}

// ControlServer is one private loopback control endpoint
type ControlServer struct {
	listener net.Listener
	path     string
	token    string
	handler  ControlHandler
	cancel   context.CancelFunc
	done     chan struct{}
	stop     chan struct{}
	close    sync.Once
	handlers sync.WaitGroup
	mu       sync.Mutex
	active   map[net.Conn]struct{}
}

type controlFile struct {
	Version int    `json:"version"`
	ID      string `json:"id"`
	Address string `json:"address"`
	Token   string `json:"token"`
}

type controlRequest struct {
	Token  string `json:"token"`
	Action string `json:"action"`
}

type controlResponse struct {
	OK     bool          `json:"ok"`
	Error  string        `json:"error,omitempty"`
	Status ControlStatus `json:"status,omitempty"`
}

// StartControl starts an authenticated loopback server and writes its private descriptor
func StartControl(ctx context.Context, directory, extensionID string, handler ControlHandler) (*ControlServer, error) {
	if ctx == nil {
		return nil, errors.New("Extension control context must not be nil")
	}
	if handler == nil {
		return nil, errors.New("Extension control handler is required")
	}
	path, err := controlPath(directory, extensionID)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create Extension control directory: %w", err)
	}
	if err := os.Chmod(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("protect Extension control directory: %w", err)
	}
	if err := reserveControlFile(ctx, directory, extensionID, path); err != nil {
		return nil, err
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		_ = os.Remove(path)
		return nil, fmt.Errorf("listen for Extension control: %w", err)
	}
	token, err := randomToken()
	if err != nil {
		_ = listener.Close()
		_ = os.Remove(path)
		return nil, err
	}
	serverContext, cancel := context.WithCancel(ctx)
	server := &ControlServer{
		listener: listener,
		path:     path,
		token:    token,
		handler:  handler,
		cancel:   cancel,
		done:     make(chan struct{}),
		stop:     make(chan struct{}),
		active:   make(map[net.Conn]struct{}),
	}
	if err := writeControlFile(path, controlFile{
		Version: 1,
		ID:      extensionID,
		Address: listener.Addr().String(),
		Token:   token,
	}); err != nil {
		cancel()
		_ = listener.Close()
		_ = os.Remove(path)
		return nil, err
	}
	go server.serve(serverContext)
	return server, nil
}

// Close stops the control endpoint and removes its matching descriptor
func (server *ControlServer) Close() error {
	var closeErr error
	server.close.Do(func() {
		close(server.stop)
		server.cancel()
		if err := server.listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			closeErr = err
		}
		<-server.done
		server.mu.Lock()
		for connection := range server.active {
			_ = connection.Close()
		}
		server.mu.Unlock()
		server.handlers.Wait()
		data, err := os.ReadFile(server.path)
		if err == nil {
			var descriptor controlFile
			if json.Unmarshal(data, &descriptor) == nil && descriptor.Token == server.token {
				if err := os.Remove(server.path); err != nil && !errors.Is(err, os.ErrNotExist) {
					closeErr = errors.Join(closeErr, fmt.Errorf("remove Extension control descriptor: %w", err))
				}
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			closeErr = errors.Join(closeErr, fmt.Errorf("read Extension control descriptor: %w", err))
		}
	})
	return closeErr
}

func (server *ControlServer) serve(ctx context.Context) {
	defer close(server.done)
	go func() {
		select {
		case <-ctx.Done():
			_ = server.listener.Close()
		case <-server.stop:
		}
	}()
	for {
		connection, err := server.listener.Accept()
		if err != nil {
			return
		}
		server.mu.Lock()
		server.active[connection] = struct{}{}
		server.handlers.Add(1)
		server.mu.Unlock()
		go func() {
			defer server.handlers.Done()
			defer func() {
				server.mu.Lock()
				delete(server.active, connection)
				server.mu.Unlock()
			}()
			server.handle(ctx, connection)
		}()
	}
}

func (server *ControlServer) handle(ctx context.Context, connection net.Conn) {
	defer connection.Close()
	if address, ok := connection.RemoteAddr().(*net.TCPAddr); !ok || !address.IP.IsLoopback() {
		return
	}
	_ = connection.SetDeadline(time.Now().Add(2 * time.Minute))
	data, err := io.ReadAll(io.LimitReader(connection, maximumControlBytes+1))
	if err != nil || len(data) > maximumControlBytes {
		return
	}
	var request controlRequest
	if err := jsonstrict.Decode(data, maximumControlBytes, &request); err != nil {
		_ = json.NewEncoder(connection).Encode(controlResponse{Error: "invalid control request"})
		return
	}
	if subtle.ConstantTimeCompare([]byte(request.Token), []byte(server.token)) != 1 {
		_ = json.NewEncoder(connection).Encode(controlResponse{Error: "control authentication failed"})
		return
	}
	var status ControlStatus
	switch request.Action {
	case "reload":
		status, err = server.handler.Reload(ctx)
	case "status":
		status, err = server.handler.Status(ctx)
	default:
		err = fmt.Errorf("unsupported control action %q", request.Action)
	}
	response := controlResponse{OK: err == nil, Status: status}
	if err != nil {
		response.Error = err.Error()
	}
	_ = json.NewEncoder(connection).Encode(response)
}

// RequestControl sends one authenticated request to a running development host
func RequestControl(ctx context.Context, directory, extensionID, action string) (ControlStatus, error) {
	if ctx == nil {
		return ControlStatus{}, errors.New("Extension control context must not be nil")
	}
	if action != "reload" && action != "status" {
		return ControlStatus{}, fmt.Errorf("unsupported Extension control action %q", action)
	}
	path, err := controlPath(directory, extensionID)
	if err != nil {
		return ControlStatus{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ControlStatus{}, fmt.Errorf("read Extension control descriptor: %w", err)
	}
	var descriptor controlFile
	if err := jsonstrict.Decode(data, maximumControlBytes, &descriptor); err != nil {
		return ControlStatus{}, fmt.Errorf("decode Extension control descriptor: %w", err)
	}
	if descriptor.Version != 1 || descriptor.ID != extensionID || descriptor.Token == "" {
		return ControlStatus{}, errors.New("Extension control descriptor is invalid")
	}
	host, _, err := net.SplitHostPort(descriptor.Address)
	if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
		return ControlStatus{}, errors.New("Extension control address is not loopback")
	}
	dialer := net.Dialer{Timeout: 2 * time.Second}
	connection, err := dialer.DialContext(ctx, "tcp", descriptor.Address)
	if err != nil {
		return ControlStatus{}, fmt.Errorf("connect to Extension development host: %w", err)
	}
	defer connection.Close()
	deadline := time.Now().Add(2 * time.Minute)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	_ = connection.SetDeadline(deadline)
	request, err := json.Marshal(controlRequest{Token: descriptor.Token, Action: action})
	if err != nil {
		return ControlStatus{}, err
	}
	if _, err := connection.Write(request); err != nil {
		return ControlStatus{}, fmt.Errorf("write Extension control request: %w", err)
	}
	if tcp, ok := connection.(*net.TCPConn); ok {
		_ = tcp.CloseWrite()
	}
	responseData, err := io.ReadAll(io.LimitReader(connection, maximumControlBytes+1))
	if err != nil || len(responseData) > maximumControlBytes {
		return ControlStatus{}, errors.New("read Extension control response failed or exceeded its limit")
	}
	var response controlResponse
	if err := jsonstrict.Decode(responseData, maximumControlBytes, &response); err != nil {
		return ControlStatus{}, fmt.Errorf("decode Extension control response: %w", err)
	}
	if !response.OK {
		if response.Error == "" {
			response.Error = "Extension control request failed"
		}
		return ControlStatus{}, errors.New(response.Error)
	}
	return response.Status, nil
}

func controlPath(directory, extensionID string) (string, error) {
	if strings.TrimSpace(directory) == "" || strings.IndexByte(directory, 0) >= 0 {
		return "", errors.New("Extension control directory is required and must not contain NUL")
	}
	if extensionID == "" || strings.TrimSpace(extensionID) != extensionID || strings.IndexByte(extensionID, 0) >= 0 {
		return "", errors.New("Extension control ID is required and must not have surrounding whitespace or NUL")
	}
	absolute, err := filepath.Abs(directory)
	if err != nil {
		return "", fmt.Errorf("resolve Extension control directory: %w", err)
	}
	digest := sha256.Sum256([]byte(extensionID))
	return filepath.Join(absolute, hex.EncodeToString(digest[:])+".json"), nil
}

func randomToken() (string, error) {
	var value [32]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("create Extension control token: %w", err)
	}
	return hex.EncodeToString(value[:]), nil
}

func writeControlFile(path string, descriptor controlFile) error {
	data, err := json.Marshal(descriptor)
	if err != nil {
		return fmt.Errorf("encode Extension control descriptor: %w", err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("open reserved Extension control descriptor: %w", err)
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return fmt.Errorf("write Extension control descriptor: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("sync Extension control descriptor: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close Extension control descriptor: %w", err)
	}
	return nil
}

func reserveControlFile(ctx context.Context, directory, extensionID, path string) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err == nil {
		return file.Close()
	}
	if !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("reserve Extension control descriptor: %w", err)
	}
	previous, readErr := os.ReadFile(path)
	if readErr != nil {
		return fmt.Errorf("read existing Extension control descriptor: %w", readErr)
	}
	if _, requestErr := RequestControl(ctx, directory, extensionID, "status"); requestErr == nil {
		return fmt.Errorf("Extension %q already has a running development host", extensionID)
	}
	current, readErr := os.ReadFile(path)
	if readErr != nil {
		return fmt.Errorf("re-read stale Extension control descriptor: %w", readErr)
	}
	if !bytes.Equal(previous, current) {
		return errors.New("Extension control descriptor changed while checking stale state")
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove stale Extension control descriptor: %w", err)
	}
	file, err = os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("reserve Extension control descriptor after stale cleanup: %w", err)
	}
	return file.Close()
}
