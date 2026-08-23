//go:build darwin

package control

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const (
	controlRequestTimeout = 35 * time.Second
	maxConnections        = 16
)

// ReloadHandler performs one reload and returns the digest that is active only
// after the worker result and supervisor state update are complete.
type ReloadHandler func(context.Context, string) (string, error)

type socketIdentity struct {
	device uint64
	inode  uint64
}

// Server owns one root-side Unix control socket.
type Server struct {
	path            string
	ownerUID        uint32
	identity        socketIdentity
	listener        *net.UnixListener
	handler         ReloadHandler
	ctx             context.Context
	cancel          context.CancelFunc
	done            chan struct{}
	connections     sync.WaitGroup
	connectionMutex sync.Mutex
	active          map[*net.UnixConn]struct{}
	connectionSlots chan struct{}
	closeOnce       sync.Once
	closeErr        error
}

// Start creates a mode-0600 Unix socket and accepts requests only from the
// expected effective UID. Production callers pass UID 0; tests can pass their
// own UID without weakening the production contract.
func Start(path string, expectedUID uint32, handler ReloadHandler) (*Server, error) {
	if err := validateSocketPath(path); err != nil {
		return nil, err
	}
	if handler == nil {
		return nil, errors.New("control reload handler is required")
	}
	if err := validateParent(path, expectedUID); err != nil {
		return nil, err
	}
	if err := RemoveStaleForOwner(path, expectedUID); err != nil {
		return nil, err
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, fmt.Errorf("listen control socket %q: %w", path, err)
	}
	listener.SetUnlinkOnClose(false)
	cleanup := func() {
		_ = listener.Close()
		_ = os.Remove(path)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		cleanup()
		return nil, fmt.Errorf("chmod control socket: %w", err)
	}
	identity, err := inspectSocket(path, expectedUID, true)
	if err != nil {
		cleanup()
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	server := &Server{
		path: path, ownerUID: expectedUID, identity: identity, listener: listener,
		handler: handler, ctx: ctx, cancel: cancel, done: make(chan struct{}), active: make(map[*net.UnixConn]struct{}),
		connectionSlots: make(chan struct{}, maxConnections),
	}
	go server.serve()
	return server, nil
}

func (server *Server) serve() {
	defer close(server.done)
	for {
		connection, err := server.listener.AcceptUnix()
		if err != nil {
			return
		}
		select {
		case server.connectionSlots <- struct{}{}:
		default:
			_ = connection.Close()
			continue
		}
		server.connectionMutex.Lock()
		server.active[connection] = struct{}{}
		server.connectionMutex.Unlock()
		server.connections.Add(1)
		go func() {
			defer server.connections.Done()
			defer func() { <-server.connectionSlots }()
			defer func() {
				server.connectionMutex.Lock()
				delete(server.active, connection)
				server.connectionMutex.Unlock()
			}()
			server.handle(connection)
		}()
	}
}

func (server *Server) handle(connection *net.UnixConn) {
	defer connection.Close() //nolint:errcheck // Best-effort connection cleanup.
	_ = connection.SetDeadline(time.Now().Add(controlRequestTimeout))
	if err := requirePeerUID(connection, server.ownerUID); err != nil {
		return
	}
	stopReadCancellation := context.AfterFunc(server.ctx, func() {
		_ = connection.SetReadDeadline(time.Now())
	})
	request, err := readRequest(connection)
	stopReadCancellation()
	if err != nil {
		_ = writeResponse(connection, ReloadResponse{Version: Version, Kind: KindReload, Error: err.Error()})
		return
	}
	requestCtx, cancel := context.WithTimeout(server.ctx, controlRequestTimeout)
	defer cancel()
	digest, reloadErr := server.handler(requestCtx, request.ExpectedConfigDigest)
	response := ReloadResponse{Version: Version, Kind: KindReload, ConfigDigest: digest}
	if reloadErr != nil {
		response.ConfigDigest = ""
		response.Error = reloadErr.Error()
		if len(response.Error) > maxErrorSize {
			response.Error = response.Error[:maxErrorSize]
		}
	}
	_ = writeResponse(connection, response)
}

func (server *Server) Close(ctx context.Context) error {
	if server == nil {
		return nil
	}
	server.closeOnce.Do(func() {
		server.cancel()
		server.closeErr = server.listener.Close()

		// Wait until the accept loop has stopped before waiting on the
		// connection group. This closes the AcceptUnix/register window and
		// guarantees that no Add can race with Wait.
		<-server.done

		connectionsDone := make(chan struct{})
		go func() {
			server.connections.Wait()
			close(connectionsDone)
		}()
		select {
		case <-connectionsDone:
		case <-ctx.Done():
			server.closeErr = errors.Join(server.closeErr, ctx.Err(), server.closeActiveConnections())
		}
		server.closeErr = errors.Join(server.closeErr, removeOwnedSocket(server.path, server.ownerUID, server.identity))
	})
	return server.closeErr
}

func (server *Server) closeActiveConnections() error {
	server.connectionMutex.Lock()
	defer server.connectionMutex.Unlock()
	var failures []error
	for connection := range server.active {
		if err := connection.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func readRequest(reader io.Reader) (ReloadRequest, error) {
	var request ReloadRequest
	payload, err := readFrame(reader)
	if err != nil {
		return request, err
	}
	if err := decodeStrict(payload, &request); err != nil {
		return request, fmt.Errorf("decode control request: %w", err)
	}
	if err := request.validate(); err != nil {
		return request, err
	}
	return request, nil
}

func writeResponse(writer io.Writer, response ReloadResponse) error {
	if err := response.validate(); err != nil {
		return err
	}
	return writeFrame(writer, response)
}

func readFrame(reader io.Reader) ([]byte, error) {
	limited := bufio.NewReader(io.LimitReader(reader, maxMessageSize+1))
	payload, err := limited.ReadBytes('\n')
	if len(payload) > maxMessageSize {
		return nil, fmt.Errorf("control message exceeds %d bytes", maxMessageSize)
	}
	if err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("control message must end with a newline")
		}
		return nil, fmt.Errorf("read control message: %w", err)
	}
	return bytes.TrimSuffix(payload, []byte{'\n'}), nil
}

func writeFrame(writer io.Writer, value any) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode control message: %w", err)
	}
	if len(payload)+1 > maxMessageSize {
		return fmt.Errorf("control message exceeds %d bytes", maxMessageSize)
	}
	payload = append(payload, '\n')
	if _, err := writer.Write(payload); err != nil {
		return fmt.Errorf("write control message: %w", err)
	}
	return nil
}

func decodeStrict(payload []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("trailing data is not allowed")
	}
	return nil
}

func requirePeerUID(connection *net.UnixConn, expectedUID uint32) error {
	raw, err := connection.SyscallConn()
	if err != nil {
		return fmt.Errorf("access control connection descriptor: %w", err)
	}
	var credential *unix.Xucred
	var socketErr error
	if err := raw.Control(func(fd uintptr) {
		credential, socketErr = unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
	}); err != nil {
		return fmt.Errorf("inspect control peer credentials: %w", err)
	}
	if socketErr != nil {
		return fmt.Errorf("inspect control peer credentials: %w", socketErr)
	}
	if credential == nil || credential.Uid != expectedUID {
		actual := uint32(^uint32(0))
		if credential != nil {
			actual = credential.Uid
		}
		return fmt.Errorf("control peer UID is %d, want %d", actual, expectedUID)
	}
	return nil
}

func validateSocketPath(path string) error {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return fmt.Errorf("control socket must be a clean absolute path: %q", path)
	}
	if len(path) > 103 {
		return fmt.Errorf("control socket exceeds the macOS path limit: %q", path)
	}
	return nil
}

func validateParent(path string, expectedUID uint32) error {
	parent := filepath.Dir(path)
	info, err := os.Lstat(parent)
	if err != nil {
		return fmt.Errorf("inspect control socket parent %q: %w", parent, err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || !ok || stat.Uid != expectedUID || info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("control socket parent %q must be a UID %d-owned non-writable directory", parent, expectedUID)
	}
	return nil
}

func inspectSocket(path string, expectedUID uint32, requireMode bool) (socketIdentity, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return socketIdentity{}, fmt.Errorf("inspect control socket %q: %w", path, err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if info.Mode()&os.ModeSocket == 0 || info.Mode()&os.ModeSymlink != 0 || !ok {
		return socketIdentity{}, fmt.Errorf("control path %q is not a Unix socket", path)
	}
	if stat.Uid != expectedUID {
		return socketIdentity{}, fmt.Errorf("control socket %q is owned by UID %d, want %d", path, stat.Uid, expectedUID)
	}
	if requireMode && info.Mode().Perm() != 0o600 {
		return socketIdentity{}, fmt.Errorf("control socket %q permissions are %04o, want 0600", path, info.Mode().Perm())
	}
	return socketIdentity{device: uint64(stat.Dev), inode: stat.Ino}, nil
}

// RemoveStaleForOwner removes only an allowed-owner Unix socket. Other object
// types and owners are left untouched.
func RemoveStaleForOwner(path string, expectedUID uint32) error {
	if err := validateSocketPath(path); err != nil {
		return err
	}
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("inspect stale control socket: %w", err)
	}
	if _, err := inspectSocket(path, expectedUID, false); err != nil {
		return fmt.Errorf("refuse to remove stale control socket: %w", err)
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove stale control socket: %w", err)
	}
	return nil
}

func removeOwnedSocket(path string, expectedUID uint32, want socketIdentity) error {
	got, err := inspectSocket(path, expectedUID, false)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		if _, statErr := os.Lstat(path); errors.Is(statErr, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("refuse to remove replaced control socket: %w", err)
	}
	if got != want {
		return fmt.Errorf("refuse to remove replaced control socket %q", path)
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove control socket: %w", err)
	}
	return nil
}
