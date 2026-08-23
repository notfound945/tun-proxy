//go:build darwin

package control

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	testRequestID   = "0123456789abcdef0123456789abcdef"
	testOperationID = "fedcba9876543210fedcba9876543210"
)

const testDigest = "sha256:09bfcc6a14b83e2192b8673677725c84883ee9cd0c70e45c9ec09daa8f2b2847"

func testReloadRequest() ReloadRequest {
	return ReloadRequest{
		Version: Version, Kind: KindReload, RequestID: testRequestID,
		OperationID: testOperationID, ExpectedConfigDigest: testDigest,
	}
}

func TestControlServerIsOwnerOnlyAndReturnsFinalReloadResult(t *testing.T) {
	path := testSocketPath(t)
	server, err := Start(path, uint32(os.Geteuid()), func(_ context.Context, request ReloadRequest) (string, error) {
		if request.ExpectedConfigDigest != testDigest {
			return "", fmt.Errorf("handler digest = %q", request.ExpectedConfigDigest)
		}
		return request.ExpectedConfigDigest, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeTestServer(t, server) })

	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSocket == 0 || info.Mode().Perm() != 0o600 {
		t.Fatalf("control socket mode = %v, want socket 0600", info.Mode())
	}
	identity, err := inspectSocket(path, uint32(os.Geteuid()), true)
	if err != nil {
		t.Fatal(err)
	}
	if identity.inode == 0 {
		t.Fatal("control socket inode was not captured")
	}

	response, err := Reload(t.Context(), path, uint32(os.Geteuid()), testReloadRequest())
	if err != nil {
		t.Fatal(err)
	}
	if response.ConfigDigest != testDigest || response.Error != "" {
		t.Fatalf("response = %+v", response)
	}
}

func TestControlServerReturnsHandlerFailure(t *testing.T) {
	path := testSocketPath(t)
	server, err := Start(path, uint32(os.Geteuid()), func(context.Context, ReloadRequest) (string, error) {
		return "", errors.New("worker rejected immutable setting")
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeTestServer(t, server) })

	response, err := Reload(t.Context(), path, uint32(os.Geteuid()), testReloadRequest())
	if err == nil || !strings.Contains(err.Error(), "worker rejected immutable setting") {
		t.Fatalf("Reload() response=%+v error=%v", response, err)
	}
	if response.Error != "worker rejected immutable setting" || response.ConfigDigest != "" {
		t.Fatalf("response = %+v", response)
	}
}

func TestControlPeerCredentialRejectsUnexpectedUID(t *testing.T) {
	path := testSocketPath(t)
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	listener.SetUnlinkOnClose(false)
	defer func() {
		_ = listener.Close()
		_ = os.Remove(path)
	}()
	accepted := make(chan error, 1)
	go func() {
		connection, acceptErr := listener.AcceptUnix()
		if acceptErr != nil {
			accepted <- acceptErr
			return
		}
		defer connection.Close() //nolint:errcheck // Test cleanup.
		accepted <- requirePeerUID(connection, uint32(os.Geteuid()+1))
	}()
	connection, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close() //nolint:errcheck // Test cleanup.
	if err := <-accepted; err == nil || !strings.Contains(err.Error(), "want") {
		t.Fatalf("requirePeerUID() error = %v", err)
	}
}

func TestControlRequestRejectsUnknownTrailingAndOversizedData(t *testing.T) {
	valid := fmt.Sprintf(`{"version":%d,"kind":"reload","request_id":%q,"operation_id":%q,"expected_config_digest":%q}`, Version, testRequestID, testOperationID, testDigest)
	tests := []struct {
		name    string
		payload string
		want    string
	}{
		{name: "unknown field", payload: strings.TrimSuffix(valid, "}") + `,"extra":true}` + "\n", want: "unknown field"},
		{name: "trailing data", payload: valid + ` {}` + "\n", want: "trailing data"},
		{name: "missing newline", payload: valid, want: "newline"},
		{name: "invalid digest", payload: fmt.Sprintf(`{"version":%d,"kind":"reload","request_id":%q,"operation_id":%q,"expected_config_digest":"sha256:nope"}`, Version, testRequestID, testOperationID) + "\n", want: "64 hex"},
		{name: "oversized", payload: strings.Repeat("x", maxMessageSize+1), want: "exceeds"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := readRequest(strings.NewReader(test.payload))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("readRequest() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestControlStartRefusesUnsafeStalePaths(t *testing.T) {
	t.Run("regular file", func(t *testing.T) {
		path := testSocketPath(t)
		if err := os.WriteFile(path, []byte("keep"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := Start(path, uint32(os.Geteuid()), func(context.Context, ReloadRequest) (string, error) { return testDigest, nil })
		if err == nil || !strings.Contains(err.Error(), "not a Unix socket") {
			t.Fatalf("Start() error = %v", err)
		}
		contents, readErr := os.ReadFile(path)
		if readErr != nil || string(contents) != "keep" {
			t.Fatalf("unsafe path changed: contents=%q error=%v", contents, readErr)
		}
	})

	t.Run("symlink", func(t *testing.T) {
		path := testSocketPath(t)
		target := path + ".target"
		if err := os.WriteFile(target, []byte("keep"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, path); err != nil {
			t.Fatal(err)
		}
		_, err := Start(path, uint32(os.Geteuid()), func(context.Context, ReloadRequest) (string, error) { return testDigest, nil })
		if err == nil || !strings.Contains(err.Error(), "not a Unix socket") {
			t.Fatalf("Start() error = %v", err)
		}
		if info, statErr := os.Lstat(path); statErr != nil || info.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("symlink was removed: mode=%v error=%v", info.Mode(), statErr)
		}
	})

	t.Run("unexpected owner", func(t *testing.T) {
		path := testSocketPath(t)
		listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
		if err != nil {
			t.Fatal(err)
		}
		listener.SetUnlinkOnClose(false)
		if err := listener.Close(); err != nil {
			t.Fatal(err)
		}
		err = RemoveStaleForOwner(path, uint32(os.Geteuid()+1))
		if err == nil || !strings.Contains(err.Error(), "owned by UID") {
			t.Fatalf("RemoveStaleForOwner() error = %v", err)
		}
		if _, statErr := os.Lstat(path); statErr != nil {
			t.Fatalf("unexpected-owner socket was removed: %v", statErr)
		}
	})
}

func TestControlCloseDoesNotRemoveReplacement(t *testing.T) {
	path := testSocketPath(t)
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	listener.SetUnlinkOnClose(false)
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	identity, err := inspectSocket(path, uint32(os.Geteuid()), true)
	if err != nil {
		t.Fatal(err)
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := removeOwnedSocket(path, uint32(os.Geteuid()), identity); err == nil || !strings.Contains(err.Error(), "refuse") {
		t.Fatalf("removeOwnedSocket() error = %v", err)
	}
	if contents, err := os.ReadFile(path); err != nil || string(contents) != "replacement" {
		t.Fatalf("replacement changed: contents=%q error=%v", contents, err)
	}
}

func TestControlCloseInterruptsIncompleteClient(t *testing.T) {
	path := testSocketPath(t)
	server, err := Start(path, uint32(os.Geteuid()), func(context.Context, ReloadRequest) (string, error) { return testDigest, nil })
	if err != nil {
		t.Fatal(err)
	}
	connection, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close() //nolint:errcheck // Test cleanup.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := server.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("control socket remains after close: %v", err)
	}
}

func testSocketPath(t *testing.T) string {
	t.Helper()
	directory, err := os.MkdirTemp("/tmp", "tp-control-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	path := filepath.Join(directory, "control.sock")
	if len(path) > 103 {
		t.Fatalf("test socket path too long: %q", path)
	}
	return path
}

func closeTestServer(t *testing.T, server *Server) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := server.Close(ctx); err != nil {
		t.Errorf("Close() error = %v", err)
	}
}

func TestControlServerRemovesStaleOwnedSocketBeforeRestart(t *testing.T) {
	path := testSocketPath(t)
	stale, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	stale.SetUnlinkOnClose(false)
	if err := stale.Close(); err != nil {
		t.Fatal(err)
	}

	server, err := Start(path, uint32(os.Geteuid()), func(context.Context, ReloadRequest) (string, error) {
		return testDigest, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	closeTestServer(t, server)
}

func TestControlClientRejectsUnsafeSocketMetadata(t *testing.T) {
	t.Run("wrong mode", func(t *testing.T) {
		path := testSocketPath(t)
		listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
		if err != nil {
			t.Fatal(err)
		}
		listener.SetUnlinkOnClose(false)
		defer func() {
			_ = listener.Close()
			_ = os.Remove(path)
		}()
		if err := os.Chmod(path, 0o660); err != nil {
			t.Fatal(err)
		}
		_, err = Reload(t.Context(), path, uint32(os.Geteuid()), testReloadRequest())
		if err == nil || !strings.Contains(err.Error(), "permissions") {
			t.Fatalf("Reload() error = %v", err)
		}
	})

	t.Run("wrong owner", func(t *testing.T) {
		path := testSocketPath(t)
		listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
		if err != nil {
			t.Fatal(err)
		}
		listener.SetUnlinkOnClose(false)
		defer func() {
			_ = listener.Close()
			_ = os.Remove(path)
		}()
		if err := os.Chmod(path, 0o600); err != nil {
			t.Fatal(err)
		}
		_, err = Reload(t.Context(), path, uint32(os.Geteuid()+1), testReloadRequest())
		if err == nil || !strings.Contains(err.Error(), "owned by UID") {
			t.Fatalf("Reload() error = %v", err)
		}
	})
}

func TestControlClientRejectsMalformedResponses(t *testing.T) {
	started := time.Now().UTC().Add(-time.Second).Format(time.RFC3339Nano)
	completed := time.Now().UTC().Format(time.RFC3339Nano)
	valid := fmt.Sprintf(`{"version":%d,"kind":%q,"request_id":%q,"result":%q,"config_digest":%q,"started_at":%q,"completed_at":%q}`,
		Version, KindReloadResult, testRequestID, ResultSucceeded, testDigest, started, completed)
	tests := []struct {
		name    string
		payload string
		want    string
	}{
		{name: "unknown field", payload: strings.TrimSuffix(valid, "}") + `,"extra":true}` + "\n", want: "unknown field"},
		{name: "trailing data", payload: valid + ` {}` + "\n", want: "trailing data"},
		{name: "missing digest", payload: fmt.Sprintf(`{"version":%d,"kind":%q,"request_id":%q,"result":%q,"started_at":%q,"completed_at":%q}`, Version, KindReloadResult, testRequestID, ResultSucceeded, started, completed) + "\n", want: "64 hex"},
		{name: "failure with digest", payload: strings.Replace(valid, `"result":"succeeded"`, `"result":"failed","error":"failed"`, 1) + "\n", want: "must not include"},
		{name: "wrong version", payload: strings.Replace(valid, fmt.Sprintf(`"version":%d`, Version), `"version":99`, 1) + "\n", want: "version"},
		{name: "wrong kind", payload: strings.Replace(valid, `"kind":"reload_result"`, `"kind":"other"`, 1) + "\n", want: "kind"},
		{name: "wrong request ID", payload: strings.Replace(valid, testRequestID, testOperationID, 1) + "\n", want: "request ID"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := testSocketPath(t)
			served := serveRawControlResponse(t, path, test.payload)
			_, err := Reload(t.Context(), path, uint32(os.Geteuid()), testReloadRequest())
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Reload() error = %v, want %q", err, test.want)
			}
			if err := <-served; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestControlCloseAllowsFinalResponseToFlush(t *testing.T) {
	path := testSocketPath(t)
	handlerStarted := make(chan struct{})
	releaseHandler := make(chan struct{})
	server, err := Start(path, uint32(os.Geteuid()), func(context.Context, ReloadRequest) (string, error) {
		close(handlerStarted)
		<-releaseHandler
		return "", errors.New("final worker rejection")
	})
	if err != nil {
		t.Fatal(err)
	}

	clientDone := make(chan error, 1)
	go func() {
		_, clientErr := Reload(context.Background(), path, uint32(os.Geteuid()), testReloadRequest())
		clientDone <- clientErr
	}()
	<-handlerStarted

	closeDone := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		closeDone <- server.Close(ctx)
	}()
	close(releaseHandler)

	if err := <-clientDone; err == nil || !strings.Contains(err.Error(), "final worker rejection") {
		t.Fatalf("Reload() error = %v", err)
	}
	if err := <-closeDone; err != nil {
		t.Fatalf("Close() error = %v", err)
	}
}

func serveRawControlResponse(t *testing.T, path, payload string) <-chan error {
	t.Helper()
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	listener.SetUnlinkOnClose(false)
	if err := os.Chmod(path, 0o600); err != nil {
		_ = listener.Close()
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() {
		defer func() {
			_ = listener.Close()
			_ = os.Remove(path)
		}()
		connection, acceptErr := listener.AcceptUnix()
		if acceptErr != nil {
			served <- acceptErr
			return
		}
		defer connection.Close() //nolint:errcheck // Test cleanup.
		if _, readErr := readFrame(connection); readErr != nil {
			served <- readErr
			return
		}
		_, writeErr := connection.Write([]byte(payload))
		served <- writeErr
	}()
	return served
}

func TestControlServerLimitsConcurrentConnections(t *testing.T) {
	path := testSocketPath(t)
	server, err := Start(path, uint32(os.Geteuid()), func(context.Context, ReloadRequest) (string, error) {
		return testDigest, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer closeTestServer(t, server)

	connections := make([]net.Conn, 0, maxConnections)
	defer func() {
		for _, connection := range connections {
			_ = connection.Close()
		}
	}()
	for i := 0; i < maxConnections; i++ {
		connection, err := net.Dial("unix", path)
		if err != nil {
			t.Fatal(err)
		}
		connections = append(connections, connection)
		waitForActiveConnections(t, server, i+1)
	}

	overflow, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer overflow.Close() //nolint:errcheck // Test cleanup.
	if err := overflow.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, 1)
	if _, err := overflow.Read(buffer); err == nil {
		t.Fatal("overflow control connection remained open")
	}
}

func waitForActiveConnections(t *testing.T, server *Server, want int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		server.connectionMutex.Lock()
		got := len(server.active)
		server.connectionMutex.Unlock()
		if got == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("active control connections did not reach %d", want)
}

func TestControlServerDeduplicatesRunningAndCompletedRequests(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	calls := 0
	server := &Server{
		ctx: context.Background(),
		handler: func(_ context.Context, request ReloadRequest) (string, error) {
			calls++
			close(started)
			<-release
			return request.ExpectedConfigDigest, nil
		},
		requests: make(map[string]*reloadRecord),
	}
	request := testReloadRequest()
	first := make(chan ReloadResponse, 1)
	go func() { first <- server.processReload(request) }()
	<-started

	running := server.processReload(request)
	if running.Result != ResultRunning || running.RequestID != request.RequestID {
		t.Fatalf("running duplicate = %+v", running)
	}
	close(release)
	final := <-first
	if final.Result != ResultSucceeded || final.ConfigDigest != testDigest {
		t.Fatalf("final response = %+v", final)
	}
	cached := server.processReload(request)
	if cached != final {
		t.Fatalf("cached response = %+v, want %+v", cached, final)
	}
	if calls != 1 {
		t.Fatalf("handler calls = %d, want 1", calls)
	}

	conflict := request
	conflict.OperationID = "11111111111111111111111111111111"
	response := server.processReload(conflict)
	if response.Result != ResultFailed || !strings.Contains(response.Error, "conflicts") {
		t.Fatalf("conflict response = %+v", response)
	}
}

func TestControlServerRecoversResultAfterClientDisconnect(t *testing.T) {
	path := testSocketPath(t)
	started := make(chan struct{})
	release := make(chan struct{})
	calls := 0
	server, err := Start(path, uint32(os.Geteuid()), func(_ context.Context, request ReloadRequest) (string, error) {
		calls++
		close(started)
		<-release
		return request.ExpectedConfigDigest, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeTestServer(t, server) })

	connection, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	request := testReloadRequest()
	if err := writeFrame(connection, request); err != nil {
		t.Fatal(err)
	}
	if err := connection.Close(); err != nil {
		t.Fatal(err)
	}
	<-started
	close(release)

	deadline := time.Now().Add(time.Second)
	for {
		response, reloadErr := Reload(t.Context(), path, uint32(os.Geteuid()), request)
		if reloadErr == nil && response.Result == ResultSucceeded {
			if response.ConfigDigest != testDigest || calls != 1 {
				t.Fatalf("response=%+v calls=%d", response, calls)
			}
			return
		}
		if reloadErr != nil || time.Now().After(deadline) {
			t.Fatalf("recover Reload() response=%+v error=%v", response, reloadErr)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestControlServerPreservesCachedDuplicateAtCapacity(t *testing.T) {
	now := time.Now().UTC()
	request := testReloadRequest()
	server := &Server{ctx: context.Background(), requests: make(map[string]*reloadRecord)}
	for index := 0; index < requestCacheLimit; index++ {
		requestID := fmt.Sprintf("%032x", index+1)
		done := make(chan struct{})
		close(done)
		recordRequest := request
		recordRequest.RequestID = requestID
		server.requests[requestID] = &reloadRecord{
			request: recordRequest, started: now.Add(-time.Minute), done: done,
			response: ReloadResponse{
				Version: Version, Kind: KindReloadResult, RequestID: requestID,
				Result: ResultSucceeded, ConfigDigest: testDigest,
				StartedAt: now.Add(-time.Minute), CompletedAt: now.Add(-time.Second),
			},
		}
	}
	request.RequestID = fmt.Sprintf("%032x", 1)
	want := server.requests[request.RequestID].response
	got := server.processReload(request)
	if got != want || len(server.requests) != requestCacheLimit {
		t.Fatalf("cached duplicate=%+v want=%+v cache=%d", got, want, len(server.requests))
	}
}

func TestControlRequestCachePrunesExpiredAndOldestCompletedRecords(t *testing.T) {
	now := time.Now().UTC()
	server := &Server{requests: make(map[string]*reloadRecord)}
	for index := 0; index < requestCacheLimit; index++ {
		requestID := fmt.Sprintf("%032x", index+1)
		done := make(chan struct{})
		close(done)
		server.requests[requestID] = &reloadRecord{
			done:     done,
			response: ReloadResponse{CompletedAt: now.Add(-time.Duration(index+1) * time.Second)},
		}
	}
	server.pruneRequestsLocked(now)
	if len(server.requests) != requestCacheLimit-1 {
		t.Fatalf("cache size after capacity prune = %d, want %d", len(server.requests), requestCacheLimit-1)
	}

	expiredID := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	done := make(chan struct{})
	close(done)
	server.requests[expiredID] = &reloadRecord{
		done:     done,
		response: ReloadResponse{CompletedAt: now.Add(-requestCacheTTL)},
	}
	server.pruneRequestsLocked(now)
	if _, exists := server.requests[expiredID]; exists {
		t.Fatal("expired reload result was not pruned")
	}
}
