package launchservice

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/hailinpan/tun-proxy/internal/privsep"
)

type memoryAccountStore struct {
	user             *accountUser
	group            *accountGroup
	used             map[uint32]struct{}
	createUserErr    error
	deleteGroupErr   error
	createdUserCount int
}

type recordingDSCLRunner struct {
	calls   [][]string
	respond func([]string) ([]byte, error)
}

func (runner *recordingDSCLRunner) Run(_ context.Context, executable string, args ...string) ([]byte, error) {
	call := append([]string{executable}, args...)
	runner.calls = append(runner.calls, call)
	if runner.respond == nil {
		return nil, nil
	}
	return runner.respond(call)
}

func (store *memoryAccountStore) LookupUser(context.Context, string) (accountUser, bool, error) {
	if store.user == nil {
		return accountUser{}, false, nil
	}
	return *store.user, true, nil
}

func (store *memoryAccountStore) LookupGroup(context.Context, string) (accountGroup, bool, error) {
	if store.group == nil {
		return accountGroup{}, false, nil
	}
	return *store.group, true, nil
}

func (store *memoryAccountStore) UsedIDs(context.Context) (map[uint32]struct{}, error) {
	result := make(map[uint32]struct{}, len(store.used))
	for id := range store.used {
		result[id] = struct{}{}
	}
	return result, nil
}

func (store *memoryAccountStore) CreateGroup(_ context.Context, group accountGroup) error {
	copy := group
	store.group = &copy
	return nil
}

func (store *memoryAccountStore) CreateUser(_ context.Context, user accountUser) error {
	store.createdUserCount++
	if store.createUserErr != nil {
		return store.createUserErr
	}
	copy := user
	store.user = &copy
	return nil
}

func (store *memoryAccountStore) DeleteUser(context.Context, string) error {
	store.user = nil
	return nil
}

func (store *memoryAccountStore) DeleteGroup(context.Context, string) error {
	if store.deleteGroupErr != nil {
		return store.deleteGroupErr
	}
	store.group = nil
	return nil
}

func TestDedicatedAccountsCreatesHighestUnusedID(t *testing.T) {
	store := &memoryAccountStore{used: map[uint32]struct{}{499: {}, 498: {}}}
	accounts := &dedicatedAccounts{store: store}
	identity, created, err := accounts.Ensure(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !created || identity.UID != 497 || identity.GID != 497 {
		t.Fatalf("Ensure() = %+v, created=%t", identity, created)
	}
	if store.user == nil || store.group == nil || store.user.Shell != workerAccountShell || store.user.Comment != workerAccountMarker {
		t.Fatalf("created records = user=%+v group=%+v", store.user, store.group)
	}
	resolved, created, err := accounts.Ensure(t.Context())
	if err != nil || created || resolved != identity {
		t.Fatalf("second Ensure() = %+v, created=%t, err=%v", resolved, created, err)
	}
}

func TestDedicatedAccountsRejectsForeignOwnershipMarker(t *testing.T) {
	user, group := testAccountRecords(493)
	user.Comment = "foreign-service"
	group.Comment = "foreign-service"
	accounts := &dedicatedAccounts{store: &memoryAccountStore{user: &user, group: &group}}
	if _, _, err := accounts.Ensure(t.Context()); err == nil || !strings.Contains(err.Error(), "ownership marker") {
		t.Fatalf("Ensure() error = %v", err)
	}
}

func TestDedicatedAccountsRollsBackGroupWhenUserCreationFails(t *testing.T) {
	store := &memoryAccountStore{createUserErr: errors.New("user failed")}
	accounts := &dedicatedAccounts{store: store}
	if _, _, err := accounts.Ensure(t.Context()); err == nil || !strings.Contains(err.Error(), "user failed") {
		t.Fatalf("Ensure() error = %v", err)
	}
	if store.user != nil || store.group != nil {
		t.Fatalf("partial identity remains: user=%+v group=%+v", store.user, store.group)
	}
}

func TestDedicatedAccountsRejectsPartialOrUnmarkedIdentity(t *testing.T) {
	validUser, validGroup := testAccountRecords(499)
	accounts := &dedicatedAccounts{store: &memoryAccountStore{user: &validUser}}
	if _, _, err := accounts.Ensure(t.Context()); err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("partial Ensure() error = %v", err)
	}
	validUser.Comment = "operator-owned"
	accounts = &dedicatedAccounts{store: &memoryAccountStore{user: &validUser, group: &validGroup}}
	if err := accounts.Purge(t.Context()); err == nil || !strings.Contains(err.Error(), "ownership marker") {
		t.Fatalf("Purge() error = %v", err)
	}
}

func TestDedicatedAccountsPurgeRestoresUserWhenGroupDeleteFails(t *testing.T) {
	user, group := testAccountRecords(499)
	store := &memoryAccountStore{user: &user, group: &group, deleteGroupErr: errors.New("group busy")}
	accounts := &dedicatedAccounts{store: store}
	err := accounts.Purge(t.Context())
	if err == nil || !strings.Contains(err.Error(), "group busy") {
		t.Fatalf("Purge() error = %v", err)
	}
	if store.user == nil || store.group == nil || store.createdUserCount != 1 {
		t.Fatalf("identity was not restored: user=%+v group=%+v creates=%d", store.user, store.group, store.createdUserCount)
	}
}

func TestDedicatedAccountsRestoreRecreatesExactIdentity(t *testing.T) {
	store := &memoryAccountStore{}
	accounts := &dedicatedAccounts{store: store}
	identity := privsep.Identity{
		User: privsep.ProductionUser, Group: privsep.ProductionGroup,
		UID: 493, GID: 493, Home: privsep.ProductionHome,
	}
	if err := accounts.Restore(t.Context(), identity); err != nil {
		t.Fatal(err)
	}
	resolved, err := accounts.Resolve(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if resolved != identity {
		t.Fatalf("Resolve() = %+v, want %+v", resolved, identity)
	}
	if err := accounts.Restore(t.Context(), identity); err != nil {
		t.Fatalf("idempotent Restore() error = %v", err)
	}
}

func TestDSCLStoreLookupParsesScalarAttributes(t *testing.T) {
	runner := &recordingDSCLRunner{respond: func(call []string) ([]byte, error) {
		switch strings.Join(call[1:], " ") {
		case ". -search /Users RecordName _tun-proxy":
			return []byte("_tun-proxy _tun-proxy\n"), nil
		case ". -read /Users/_tun-proxy RecordName UniqueID PrimaryGroupID NFSHomeDirectory UserShell Password Comment":
			return []byte("RecordName: _tun-proxy\nUniqueID: 493\nPrimaryGroupID: 493\nNFSHomeDirectory: /var/empty\nUserShell: /usr/bin/false\nPassword: *\nComment: cn.notfound945.tun-proxy\n"), nil
		default:
			return nil, errors.New("unexpected dscl command: " + strings.Join(call, " "))
		}
	}}
	user, exists, err := (dsclStore{runner: runner}).LookupUser(t.Context(), privsep.ProductionUser)
	if err != nil {
		t.Fatal(err)
	}
	if !exists || user.UID != 493 || user.GID != 493 || user.Home != privsep.ProductionHome || user.Comment != workerAccountMarker {
		t.Fatalf("LookupUser() = %+v, exists=%t", user, exists)
	}
	if len(runner.calls) != 2 || runner.calls[0][0] != dsclPath {
		t.Fatalf("dscl calls = %v", runner.calls)
	}
}

func TestDSCLStoreListsIDsAndBuildsCreateDeleteCommands(t *testing.T) {
	runner := &recordingDSCLRunner{respond: func(call []string) ([]byte, error) {
		joined := strings.Join(call[1:], " ")
		switch joined {
		case ". -list /Users UniqueID":
			return []byte("root 0\noperator 501\nmalformed nope\n"), nil
		case ". -list /Groups PrimaryGroupID":
			return []byte("wheel 0\n_tun-proxy 493\n"), nil
		default:
			return nil, nil
		}
	}}
	store := dsclStore{runner: runner}
	used, err := store.UsedIDs(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []uint32{0, 493, 501} {
		if _, ok := used[id]; !ok {
			t.Errorf("UsedIDs() lacks %d: %v", id, used)
		}
	}
	group := accountGroup{Name: privsep.ProductionGroup, GID: 493, Password: workerPasswordMarker, Comment: workerAccountMarker}
	if err := store.CreateGroup(t.Context(), group); err != nil {
		t.Fatal(err)
	}
	user := accountUser{
		Name: privsep.ProductionUser, UID: 493, GID: 493, Home: privsep.ProductionHome,
		Shell: workerAccountShell, Password: workerPasswordMarker, Comment: workerAccountMarker,
	}
	if err := store.CreateUser(t.Context(), user); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteUser(t.Context(), user.Name); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteGroup(t.Context(), group.Name); err != nil {
		t.Fatal(err)
	}
	wantGroupCreate := dsclPath + " . -create /Groups/_tun-proxy"
	wantUserCreate := dsclPath + " . -create /Users/_tun-proxy"
	foundGroupCreate := false
	foundUserCreate := false
	for _, call := range runner.calls {
		joined := strings.Join(call, " ")
		if joined == wantGroupCreate {
			foundGroupCreate = true
		}
		if joined == wantUserCreate {
			foundUserCreate = true
		}
		if strings.Contains(joined, " GeneratedUID ") {
			t.Fatalf("must not replace the UUID assigned by Open Directory: %q", joined)
		}
	}
	if !foundGroupCreate || !foundUserCreate {
		t.Fatalf("record create calls missing: group=%t user=%t calls=%v", foundGroupCreate, foundUserCreate, runner.calls)
	}
	last := strings.Join(runner.calls[len(runner.calls)-1], " ")
	if last != dsclPath+" . -delete /Groups/_tun-proxy" {
		t.Fatalf("delete call = %q", last)
	}
}

func TestAllocateWorkerIDExhausted(t *testing.T) {
	used := make(map[uint32]struct{})
	for id := workerIDMinimum; id <= workerIDMaximum; id++ {
		used[id] = struct{}{}
	}
	if _, err := allocateWorkerID(used); err == nil {
		t.Fatal("allocateWorkerID() accepted an exhausted range")
	}
}

func testAccountRecords(id uint32) (accountUser, accountGroup) {
	return accountUser{
			Name: privsep.ProductionUser, UID: id, GID: id, Home: privsep.ProductionHome,
			Shell: workerAccountShell, Password: workerPasswordMarker, Comment: workerAccountMarker,
		}, accountGroup{
			Name: privsep.ProductionGroup, GID: id, Password: workerPasswordMarker, Comment: workerAccountMarker,
		}
}
