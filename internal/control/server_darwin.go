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
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

const (
	controlRequestTimeout = 35 * time.Second
	maxConnections        = 16
	requestCacheLimit     = 64
	requestCacheTTL       = 10 * time.Minute
)

// ReloadHandler performs one reload and returns the digest that is active only
// after the worker result and supervisor state update are complete.
type ReloadHandler func(context.Context, ReloadRequest) (string, error)

type socketIdentity struct {
	device uint64
	inode  uint64
}

type reloadRecord struct {
	request  ReloadRequest
	started  time.Time
	done     chan struct{}
	response ReloadResponse
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
	requestMutex    sync.Mutex
	requests        map[string]*reloadRecord
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
		connectionSlots: make(chan struct{}, maxConnections), requests: make(map[string]*reloadRecord),
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
		now := time.Now().UTC()
		_ = writeResponse(connection, failedResponse(unknownRequestID, now, err))
		return
	}
	_ = writeResponse(connection, server.processReload(request))
}

func (server *Server) processReload(request ReloadRequest) ReloadResponse {
	now := time.Now().UTC()
	server.requestMutex.Lock()
	server.pruneExpiredRequestsLocked(now)
	if existing := server.requests[request.RequestID]; existing != nil {
		if !existing.request.sameIdentity(request) {
			server.requestMutex.Unlock()
			return failedResponse(request.RequestID, now, fmt.Errorf(
				"reload request ID %s conflicts with an existing request", request.RequestID,
			))
		}
		select {
		case <-existing.done:
			response := existing.response
			server.requestMutex.Unlock()
			return response
		default:
			response := ReloadResponse{
				Version: Version, Kind: KindReloadResult, RequestID: request.RequestID,
				Result: ResultRunning, StartedAt: existing.started,
			}
			server.requestMutex.Unlock()
			return response
		}
	}
	server.pruneRequestsLocked(now)
	if len(server.requests) >= requestCacheLimit {
		server.requestMutex.Unlock()
		return failedResponse(request.RequestID, now, errors.New("reload request cache is full"))
	}
	record := &reloadRecord{request: request, started: now, done: make(chan struct{})}
	server.requests[request.RequestID] = record
	server.requestMutex.Unlock()

	go server.executeReload(record)
	<-record.done
	server.requestMutex.Lock()
	response := record.response
	server.requestMutex.Unlock()
	return response
}

func (server *Server) executeReload(record *reloadRecord) {
	requestCtx, cancel := context.WithTimeout(server.ctx, controlRequestTimeout)
	digest, reloadErr := server.handler(requestCtx, record.request)
	cancel()
	completed := time.Now().UTC()
	response := ReloadResponse{
		Version: Version, Kind: KindReloadResult, RequestID: record.request.RequestID,
		Result: ResultSucceeded, ConfigDigest: digest, StartedAt: record.started, CompletedAt: completed,
	}
	if reloadErr == nil {
		if err := validateDigest(digest); err != nil {
			reloadErr = fmt.Errorf("reload handler returned an invalid digest: %w", err)
		}
	}
	if reloadErr != nil {
		response = failedResponse(record.request.RequestID, record.started, reloadErr)
		response.CompletedAt = completed
	}
	server.requestMutex.Lock()
	record.response = response
	close(record.done)
	server.requestMutex.Unlock()
}

func failedResponse(requestID string, started time.Time, err error) ReloadResponse {
	message := "reload failed"
	if err != nil {
		message = err.Error()
	}
	message = strings.ToValidUTF8(message, "�")
	if len(message) > maxErrorSize {
		message = message[:maxErrorSize]
		for !utf8.ValidString(message) {
			message = message[:len(message)-1]
		}
	}
	return ReloadResponse{
		Version: Version, Kind: KindReloadResult, RequestID: requestID,
		Result: ResultFailed, StartedAt: started, CompletedAt: time.Now().UTC(), Error: message,
	}
}

func (server *Server) pruneExpiredRequestsLocked(now time.Time) {
	for requestID, record := range server.requests {
		select {
		case <-record.done:
			if !record.response.CompletedAt.IsZero() && now.Sub(record.response.CompletedAt) >= requestCacheTTL {
				delete(server.requests, requestID)
			}
		default:
		}
	}
}

func (server *Server) pruneRequestsLocked(now time.Time) {
	server.pruneExpiredRequestsLocked(now)
	if len(server.requests) < requestCacheLimit {
		return
	}
	type completedRecord struct {
		requestID   string
		completedAt time.Time
	}
	completed := make([]completedRecord, 0, len(server.requests))
	for requestID, record := range server.requests {
		select {
		case <-record.done:
			completed = append(completed, completedRecord{requestID: requestID, completedAt: record.response.CompletedAt})
		default:
		}
	}
	sort.Slice(completed, func(i, j int) bool { return completed[i].completedAt.Before(completed[j].completedAt) })
	for _, record := range completed {
		if len(server.requests) < requestCacheLimit {
			break
		}
		delete(server.requests, record.requestID)
	}
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
