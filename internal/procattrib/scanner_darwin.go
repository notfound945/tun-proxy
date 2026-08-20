//go:build darwin && cgo

package procattrib

/*
#cgo LDFLAGS: -lproc

#include <arpa/inet.h>
#include <errno.h>
#include <libproc.h>
#include <stdint.h>
#include <string.h>
#include <sys/proc_info.h>

struct tp_socket_record {
	int family;
	int type;
	int protocol;
	int state;
	int remote_open;
	uint16_t local_port;
	uint16_t remote_port;
	uint8_t local_addr[16];
	uint8_t remote_addr[16];
	uint64_t socket_id;
	uint64_t generation;
};

static int tp_list_pids(int *buffer, int capacity, int *saved_errno) {
	errno = 0;
	int bytes = proc_listpids(PROC_ALL_PIDS, 0, buffer, capacity * (int)sizeof(int));
	*saved_errno = errno;
	if (bytes < 0 || (bytes == 0 && errno != 0)) {
		return -1;
	}
	return bytes / (int)sizeof(int);
}

static int tp_list_fds(int pid, struct proc_fdinfo *buffer, int capacity, int *saved_errno) {
	errno = 0;
	int bytes = proc_pidinfo(pid, PROC_PIDLISTFDS, 0, buffer,
		capacity * (int)sizeof(struct proc_fdinfo));
	*saved_errno = errno;
	if (bytes < 0 || (bytes == 0 && errno != 0)) {
		return -1;
	}
	return bytes / (int)sizeof(struct proc_fdinfo);
}

static int tp_socket_record(int pid, int fd, struct tp_socket_record *record, int *saved_errno) {
	struct socket_fdinfo info;
	memset(&info, 0, sizeof(info));
	errno = 0;
	int bytes = proc_pidfdinfo(pid, fd, PROC_PIDFDSOCKETINFO, &info, sizeof(info));
	*saved_errno = errno;
	if (bytes <= 0) {
		return -1;
	}
	if (bytes != sizeof(info)) {
		return -2;
	}

	memset(record, 0, sizeof(*record));
	record->family = info.psi.soi_family;
	record->type = info.psi.soi_type;
	record->protocol = info.psi.soi_protocol;
	record->socket_id = info.psi.soi_so;

	struct in_sockinfo *in = NULL;
	if (info.psi.soi_kind == SOCKINFO_TCP) {
		in = &info.psi.soi_proto.pri_tcp.tcpsi_ini;
		record->state = info.psi.soi_proto.pri_tcp.tcpsi_state;
	} else if (info.psi.soi_kind == SOCKINFO_IN) {
		in = &info.psi.soi_proto.pri_in;
	} else {
		return 0;
	}

	record->local_port = ntohs((uint16_t)in->insi_lport);
	record->remote_port = ntohs((uint16_t)in->insi_fport);
	record->generation = in->insi_gencnt;
	if ((in->insi_vflag & INI_IPV4) != 0) {
		record->family = AF_INET;
		memcpy(record->local_addr, &in->insi_laddr.ina_46.i46a_addr4, 4);
		memcpy(record->remote_addr, &in->insi_faddr.ina_46.i46a_addr4, 4);
	} else if ((in->insi_vflag & INI_IPV6) != 0) {
		record->family = AF_INET6;
		memcpy(record->local_addr, &in->insi_laddr.ina_6, 16);
		memcpy(record->remote_addr, &in->insi_faddr.ina_6, 16);
	} else {
		return 0;
	}
	record->remote_open = record->remote_port == 0;
	return 1;
}

static int tp_process_name(int pid, char *buffer, int capacity) {
	return proc_name(pid, buffer, (uint32_t)capacity);
}
*/
import "C"

import (
	"errors"
	"fmt"
	"net/netip"
	"sort"
	"syscall"
	"time"
	"unsafe"
)

const initialScanCapacity = 256

func Lookup(flow Flow) (Result, error) {
	if err := flow.Validate(); err != nil {
		return Result{}, err
	}
	started := time.Now()
	result := Result{Flow: flow}
	pids, err := listPIDs()
	if err != nil {
		return Result{}, err
	}
	seen := make(map[string]struct{})
	for _, pid := range pids {
		fds, scanErr := listFDs(pid)
		if scanErr != nil {
			switch {
			case errors.Is(scanErr, syscall.EPERM), errors.Is(scanErr, syscall.EACCES):
				result.Diagnostics.PermissionDenied++
			case errors.Is(scanErr, syscall.ESRCH):
				result.Diagnostics.ProcessesVanished++
			}
			continue
		}
		result.Diagnostics.PIDsScanned++
		name := ""
		for _, fd := range fds {
			if fd.kind != C.PROX_FDTYPE_SOCKET {
				continue
			}
			result.Diagnostics.SocketFDsScanned++
			record, recordErr := readSocket(pid, fd.number)
			if recordErr != nil {
				if errors.Is(recordErr, syscall.ESRCH) || errors.Is(recordErr, syscall.EBADF) {
					result.Diagnostics.ProcessesVanished++
				}
				continue
			}
			if record == nil || !matches(flow, *record) {
				continue
			}
			key := fmt.Sprintf("%d/%d", pid, record.socketID)
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}
			if name == "" {
				name = processName(pid)
			}
			result.Owners = append(result.Owners, Owner{
				PID: pid, FD: fd.number, Name: name,
				SocketID: record.socketID, Generation: record.generation,
			})
		}
	}
	sort.Slice(result.Owners, func(i, j int) bool {
		if result.Owners[i].PID != result.Owners[j].PID {
			return result.Owners[i].PID < result.Owners[j].PID
		}
		return result.Owners[i].FD < result.Owners[j].FD
	})
	result.Outcome = outcomeForOwners(result.Owners)
	result.Diagnostics.Duration = time.Since(started)
	return result, nil
}

type fdRecord struct {
	number int
	kind   C.uint32_t
}

func listPIDs() ([]int, error) {
	capacity := initialScanCapacity
	for {
		buffer := make([]C.int, capacity)
		var savedErrno C.int
		count := C.tp_list_pids(&buffer[0], C.int(capacity), &savedErrno)
		if count < 0 {
			return nil, fmt.Errorf("list processes: %w", syscall.Errno(savedErrno))
		}
		if int(count) < capacity {
			pids := make([]int, 0, int(count))
			for _, pid := range buffer[:count] {
				if pid > 0 {
					pids = append(pids, int(pid))
				}
			}
			return pids, nil
		}
		capacity *= 2
	}
}

func listFDs(pid int) ([]fdRecord, error) {
	capacity := 32
	for {
		buffer := make([]C.struct_proc_fdinfo, capacity)
		var savedErrno C.int
		count := C.tp_list_fds(C.int(pid), &buffer[0], C.int(capacity), &savedErrno)
		if count < 0 {
			return nil, syscall.Errno(savedErrno)
		}
		if int(count) < capacity {
			fds := make([]fdRecord, 0, int(count))
			for _, item := range buffer[:count] {
				fds = append(fds, fdRecord{number: int(item.proc_fd), kind: item.proc_fdtype})
			}
			return fds, nil
		}
		capacity *= 2
	}
}

func readSocket(pid, fd int) (*socketRecord, error) {
	var native C.struct_tp_socket_record
	var savedErrno C.int
	status := C.tp_socket_record(C.int(pid), C.int(fd), &native, &savedErrno)
	if status < 0 {
		if status == -2 {
			return nil, errors.New("proc_pidfdinfo returned a partial socket record")
		}
		return nil, syscall.Errno(savedErrno)
	}
	if status == 0 {
		return nil, nil
	}
	protocol := Protocol("")
	switch int(native.protocol) {
	case syscall.IPPROTO_TCP:
		protocol = TCP
	case syscall.IPPROTO_UDP:
		protocol = UDP
	default:
		return nil, nil
	}
	local, err := nativeAddrPort(native.family, &native.local_addr[0], native.local_port)
	if err != nil {
		return nil, err
	}
	remote, err := nativeAddrPort(native.family, &native.remote_addr[0], native.remote_port)
	if err != nil {
		return nil, err
	}
	return &socketRecord{
		protocol: protocol, local: local, remote: remote,
		remoteOpen: native.remote_open != 0,
		socketID:   uint64(native.socket_id), generation: uint64(native.generation),
	}, nil
}

func nativeAddrPort(family C.int, address *C.uint8_t, port C.uint16_t) (netip.AddrPort, error) {
	length := 0
	switch int(family) {
	case syscall.AF_INET:
		length = 4
	case syscall.AF_INET6:
		length = 16
	default:
		return netip.AddrPort{}, fmt.Errorf("unsupported socket address family %d", family)
	}
	bytes := C.GoBytes(unsafe.Pointer(address), C.int(length))
	addr, ok := netip.AddrFromSlice(bytes)
	if !ok {
		return netip.AddrPort{}, errors.New("invalid socket address bytes")
	}
	return netip.AddrPortFrom(addr.Unmap(), uint16(port)), nil
}

func processName(pid int) string {
	buffer := make([]C.char, 256)
	length := C.tp_process_name(C.int(pid), &buffer[0], C.int(len(buffer)))
	if length <= 0 {
		return ""
	}
	return C.GoStringN(&buffer[0], length)
}
