// Package privsep defines the private, versioned protocol between the
// privileged service supervisor and its non-root data-plane worker.
package privsep

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"path/filepath"
	"strings"
	"sync"
)

const (
	ProtocolVersion  = 1
	MaxConfigSize    = 1 << 20
	maxFrameSize     = 2 << 20
	maxDNSInterfaces = 32
	maxDNSServers    = 8
)

type Kind string

const (
	KindBootstrap    Kind = "bootstrap"
	KindPrepared     Kind = "prepared"
	KindCommit       Kind = "commit"
	KindRunning      Kind = "running"
	KindReload       Kind = "reload"
	KindReloadResult Kind = "reload_result"
	KindShutdown     Kind = "shutdown"
	KindStopped      Kind = "stopped"
	KindFatal        Kind = "fatal"
)

type Message struct {
	Version   int             `json:"version"`
	Kind      Kind            `json:"kind"`
	RequestID uint64          `json:"request_id,omitempty"`
	Payload   json.RawMessage `json:"payload"`
}

type Bootstrap struct {
	Config             []byte                      `json:"config"`
	ConfigDigest       string                      `json:"config_digest"`
	TUNName            string                      `json:"tun_name"`
	DNSListen          string                      `json:"dns_listen"`
	StatusSocket       string                      `json:"status_socket"`
	IPv6Enabled        bool                        `json:"ipv6_enabled"`
	IPv6FallbackReason string                      `json:"ipv6_fallback_reason,omitempty"`
	InterfaceDNS       map[string][]netip.AddrPort `json:"interface_dns,omitempty"`
}

func (bootstrap Bootstrap) Validate() error {
	var failures []error
	if len(bootstrap.Config) == 0 {
		failures = append(failures, errors.New("bootstrap config is empty"))
	} else if len(bootstrap.Config) > MaxConfigSize {
		failures = append(failures, fmt.Errorf("bootstrap config exceeds %d bytes", MaxConfigSize))
	}
	if err := validateDigest(bootstrap.ConfigDigest); err != nil {
		failures = append(failures, err)
	} else if err := validatePayloadDigest(bootstrap.Config, bootstrap.ConfigDigest); err != nil {
		failures = append(failures, err)
	}
	if !strings.HasPrefix(bootstrap.TUNName, "utun") || len(bootstrap.TUNName) <= len("utun") {
		failures = append(failures, fmt.Errorf("bootstrap TUN name is invalid: %q", bootstrap.TUNName))
	}
	listen, err := netip.ParseAddrPort(bootstrap.DNSListen)
	if err != nil || !listen.IsValid() || !listen.Addr().Is4() || !listen.Addr().IsLoopback() || listen.Port() == 0 {
		failures = append(failures, fmt.Errorf("bootstrap DNS listen address must be non-zero IPv4 loopback: %q", bootstrap.DNSListen))
	}
	if !filepath.IsAbs(bootstrap.StatusSocket) || filepath.Clean(bootstrap.StatusSocket) != bootstrap.StatusSocket {
		failures = append(failures, fmt.Errorf("bootstrap status socket must be a clean absolute path: %q", bootstrap.StatusSocket))
	} else if len(bootstrap.StatusSocket) > 103 {
		failures = append(failures, fmt.Errorf("bootstrap status socket exceeds the macOS path limit: %q", bootstrap.StatusSocket))
	}
	if bootstrap.IPv6Enabled && bootstrap.IPv6FallbackReason != "" {
		failures = append(failures, errors.New("enabled IPv6 bootstrap cannot include a fallback reason"))
	}
	if err := validateInterfaceDNS(bootstrap.InterfaceDNS); err != nil {
		failures = append(failures, err)
	}
	return errors.Join(failures...)
}

type Prepared struct {
	PID int    `json:"pid"`
	UID uint32 `json:"uid"`
	GID uint32 `json:"gid"`
}

func (prepared Prepared) Validate() error {
	if prepared.PID <= 0 {
		return fmt.Errorf("prepared worker PID must be positive, got %d", prepared.PID)
	}
	if prepared.UID == 0 || prepared.GID == 0 {
		return fmt.Errorf("prepared worker must be non-root, got uid=%d gid=%d", prepared.UID, prepared.GID)
	}
	return nil
}

type Commit struct {
	ConfigDigest string `json:"config_digest"`
}

type Running struct {
	ConfigDigest string `json:"config_digest"`
}

type Reload struct {
	Config       []byte                      `json:"config"`
	ConfigDigest string                      `json:"config_digest"`
	InterfaceDNS map[string][]netip.AddrPort `json:"interface_dns,omitempty"`
}

func (reload Reload) Validate() error {
	var failures []error
	if len(reload.Config) == 0 {
		failures = append(failures, errors.New("reload config is empty"))
	} else if len(reload.Config) > MaxConfigSize {
		failures = append(failures, fmt.Errorf("reload config exceeds %d bytes", MaxConfigSize))
	}
	if err := validateDigest(reload.ConfigDigest); err != nil {
		failures = append(failures, err)
	} else if err := validatePayloadDigest(reload.Config, reload.ConfigDigest); err != nil {
		failures = append(failures, err)
	}
	if err := validateInterfaceDNS(reload.InterfaceDNS); err != nil {
		failures = append(failures, err)
	}
	return errors.Join(failures...)
}

func validateInterfaceDNS(configured map[string][]netip.AddrPort) error {
	if len(configured) > maxDNSInterfaces {
		return fmt.Errorf("interface DNS contains %d interfaces, maximum is %d", len(configured), maxDNSInterfaces)
	}
	var failures []error
	for interfaceName, servers := range configured {
		if interfaceName == "" || len(interfaceName) > 15 || strings.HasPrefix(interfaceName, "-") {
			failures = append(failures, fmt.Errorf("interface DNS name is invalid: %q", interfaceName))
			continue
		}
		for _, character := range interfaceName {
			if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || character == '_' || character == '-' || character == '.' {
				continue
			}
			failures = append(failures, fmt.Errorf("interface DNS name is invalid: %q", interfaceName))
			break
		}
		if len(servers) == 0 || len(servers) > maxDNSServers {
			failures = append(failures, fmt.Errorf("interface DNS %q must contain 1..%d servers", interfaceName, maxDNSServers))
			continue
		}
		seen := make(map[netip.AddrPort]struct{}, len(servers))
		for _, server := range servers {
			address := server.Addr().Unmap()
			if !server.IsValid() || server.Port() != 53 || !address.IsValid() || address.IsUnspecified() || address.IsLoopback() || address.IsMulticast() {
				failures = append(failures, fmt.Errorf("interface DNS %q contains unsafe server %q", interfaceName, server))
			}
			if _, exists := seen[server]; exists {
				failures = append(failures, fmt.Errorf("interface DNS %q contains duplicate server %q", interfaceName, server))
			}
			seen[server] = struct{}{}
		}
	}
	return errors.Join(failures...)
}

type ReloadResult struct {
	ConfigDigest string `json:"config_digest,omitempty"`
	Error        string `json:"error,omitempty"`
}

type Shutdown struct {
	Reason string `json:"reason,omitempty"`
}

type Stopped struct {
	Error string `json:"error,omitempty"`
}

type Fatal struct {
	Error string `json:"error"`
}

type Codec struct {
	reader io.Reader
	writer io.Writer
	mutex  sync.Mutex
}

func NewCodec(reader io.Reader, writer io.Writer) (*Codec, error) {
	if reader == nil || writer == nil {
		return nil, errors.New("privilege-separation protocol requires a reader and writer")
	}
	return &Codec{reader: reader, writer: writer}, nil
}

func (codec *Codec) Send(kind Kind, requestID uint64, payload any) error {
	if codec == nil || codec.writer == nil {
		return errors.New("privilege-separation protocol writer is unavailable")
	}
	if !knownKind(kind) {
		return fmt.Errorf("unknown privilege-separation message kind %q", kind)
	}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode %s payload: %w", kind, err)
	}
	messageBytes, err := json.Marshal(Message{
		Version: ProtocolVersion, Kind: kind, RequestID: requestID, Payload: payloadBytes,
	})
	if err != nil {
		return fmt.Errorf("encode %s message: %w", kind, err)
	}
	if len(messageBytes) > maxFrameSize {
		return fmt.Errorf("%s message exceeds %d bytes", kind, maxFrameSize)
	}
	header := make([]byte, 4)
	binary.BigEndian.PutUint32(header, uint32(len(messageBytes)))
	codec.mutex.Lock()
	defer codec.mutex.Unlock()
	if err := writeAll(codec.writer, header); err != nil {
		return fmt.Errorf("write %s header: %w", kind, err)
	}
	if err := writeAll(codec.writer, messageBytes); err != nil {
		return fmt.Errorf("write %s message: %w", kind, err)
	}
	return nil
}

func (codec *Codec) Receive() (Message, error) {
	if codec == nil || codec.reader == nil {
		return Message{}, errors.New("privilege-separation protocol reader is unavailable")
	}
	header := make([]byte, 4)
	if _, err := io.ReadFull(codec.reader, header); err != nil {
		return Message{}, fmt.Errorf("read privilege-separation header: %w", err)
	}
	size := binary.BigEndian.Uint32(header)
	if size == 0 || size > maxFrameSize {
		return Message{}, fmt.Errorf("invalid privilege-separation frame size %d", size)
	}
	contents := make([]byte, size)
	if _, err := io.ReadFull(codec.reader, contents); err != nil {
		return Message{}, fmt.Errorf("read privilege-separation frame: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	var message Message
	if err := decoder.Decode(&message); err != nil {
		return Message{}, fmt.Errorf("decode privilege-separation message: %w", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return Message{}, err
	}
	if message.Version != ProtocolVersion {
		return Message{}, fmt.Errorf("unsupported privilege-separation protocol version %d", message.Version)
	}
	if !knownKind(message.Kind) {
		return Message{}, fmt.Errorf("unknown privilege-separation message kind %q", message.Kind)
	}
	if len(message.Payload) == 0 {
		return Message{}, fmt.Errorf("%s message has no payload", message.Kind)
	}
	return message, nil
}

func DecodePayload[T any](message Message) (T, error) {
	var result T
	decoder := json.NewDecoder(bytes.NewReader(message.Payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		return result, fmt.Errorf("decode %s payload: %w", message.Kind, err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return result, err
	}
	return result, nil
}

func ReceiveKind[T any](codec *Codec, want Kind) (T, uint64, error) {
	var zero T
	message, err := codec.Receive()
	if err != nil {
		return zero, 0, err
	}
	if message.Kind == KindFatal {
		fatal, decodeErr := DecodePayload[Fatal](message)
		if decodeErr != nil {
			return zero, 0, decodeErr
		}
		if strings.TrimSpace(fatal.Error) == "" {
			fatal.Error = "worker reported an unspecified fatal error"
		}
		return zero, message.RequestID, errors.New(fatal.Error)
	}
	if message.Kind != want {
		return zero, message.RequestID, fmt.Errorf("received %s message, want %s", message.Kind, want)
	}
	payload, err := DecodePayload[T](message)
	return payload, message.RequestID, err
}

func validateDigest(digest string) error {
	const prefix = "sha256:"
	if !strings.HasPrefix(digest, prefix) || len(digest) != len(prefix)+64 {
		return fmt.Errorf("config digest must be sha256:<64 hex characters>, got %q", digest)
	}
	if _, err := hex.DecodeString(strings.TrimPrefix(digest, prefix)); err != nil {
		return fmt.Errorf("config digest is invalid: %w", err)
	}
	return nil
}

func validatePayloadDigest(contents []byte, digest string) error {
	want := fmt.Sprintf("sha256:%x", sha256.Sum256(contents))
	if digest != want {
		return fmt.Errorf("config payload digest=%q, want %q", digest, want)
	}
	return nil
}

func knownKind(kind Kind) bool {
	switch kind {
	case KindBootstrap, KindPrepared, KindCommit, KindRunning, KindReload,
		KindReloadResult, KindShutdown, KindStopped, KindFatal:
		return true
	default:
		return false
	}
}

func writeAll(writer io.Writer, contents []byte) error {
	for len(contents) != 0 {
		count, err := writer.Write(contents)
		if err != nil {
			return err
		}
		if count <= 0 {
			return io.ErrShortWrite
		}
		contents = contents[count:]
	}
	return nil
}

func requireJSONEOF(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("trailing JSON value is not allowed")
		}
		return fmt.Errorf("decode privilege-separation message: %w", err)
	}
	return nil
}
