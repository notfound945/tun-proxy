package launchservice

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/hailinpan/tun-proxy/internal/privsep"
)

const (
	dsclPath             = "/usr/bin/dscl"
	workerAccountMarker  = "cn.notfound945.tun-proxy"
	workerAccountName    = "tun-proxy service worker"
	workerAccountShell   = "/usr/bin/false"
	workerIDMinimum      = uint32(401)
	workerIDMaximum      = uint32(499)
	workerPasswordMarker = "*"
)

// WorkerAccounts owns the lifecycle of the dedicated non-root service
// identity. Ensure reports whether it created the identity so callers can
// remove it when a larger installation transaction rolls back.
type WorkerAccounts interface {
	Ensure(context.Context) (privsep.Identity, bool, error)
	Resolve(context.Context) (privsep.Identity, error)
	Purge(context.Context) error
	Restore(context.Context, privsep.Identity) error
}

type accountUser struct {
	Name     string
	UID      uint32
	GID      uint32
	Home     string
	Shell    string
	Password string
	Comment  string
}

type accountGroup struct {
	Name     string
	GID      uint32
	Password string
	Comment  string
}

type accountStore interface {
	LookupUser(context.Context, string) (accountUser, bool, error)
	LookupGroup(context.Context, string) (accountGroup, bool, error)
	UsedIDs(context.Context) (map[uint32]struct{}, error)
	CreateGroup(context.Context, accountGroup) error
	CreateUser(context.Context, accountUser) error
	DeleteUser(context.Context, string) error
	DeleteGroup(context.Context, string) error
}

type dedicatedAccounts struct{ store accountStore }

func newWorkerAccounts(runner CommandRunner) WorkerAccounts {
	return &dedicatedAccounts{store: dsclStore{runner: runner}}
}

func (accounts *dedicatedAccounts) Resolve(ctx context.Context) (privsep.Identity, error) {
	if accounts == nil || accounts.store == nil {
		return privsep.Identity{}, errors.New("worker account store is required")
	}
	user, userExists, err := accounts.store.LookupUser(ctx, privsep.ProductionUser)
	if err != nil {
		return privsep.Identity{}, err
	}
	group, groupExists, err := accounts.store.LookupGroup(ctx, privsep.ProductionGroup)
	if err != nil {
		return privsep.Identity{}, err
	}
	if !userExists || !groupExists {
		return privsep.Identity{}, fmt.Errorf("dedicated worker identity is incomplete: user=%t group=%t", userExists, groupExists)
	}
	return validateDedicatedAccount(user, group)
}

func (accounts *dedicatedAccounts) Ensure(ctx context.Context) (identity privsep.Identity, created bool, resultErr error) {
	if accounts == nil || accounts.store == nil {
		return identity, false, errors.New("worker account store is required")
	}
	user, userExists, err := accounts.store.LookupUser(ctx, privsep.ProductionUser)
	if err != nil {
		return identity, false, err
	}
	group, groupExists, err := accounts.store.LookupGroup(ctx, privsep.ProductionGroup)
	if err != nil {
		return identity, false, err
	}
	if userExists != groupExists {
		return identity, false, fmt.Errorf("refuse incomplete dedicated worker identity: user=%t group=%t", userExists, groupExists)
	}
	if userExists {
		identity, err = validateDedicatedAccount(user, group)
		return identity, false, err
	}

	used, err := accounts.store.UsedIDs(ctx)
	if err != nil {
		return identity, false, err
	}
	id, err := allocateWorkerID(used)
	if err != nil {
		return identity, false, err
	}
	group = accountGroup{
		Name: privsep.ProductionGroup, GID: id, Password: workerPasswordMarker, Comment: workerAccountMarker,
	}
	user = accountUser{
		Name: privsep.ProductionUser, UID: id, GID: id, Home: privsep.ProductionHome,
		Shell: workerAccountShell, Password: workerPasswordMarker, Comment: workerAccountMarker,
	}
	if err := accounts.store.CreateGroup(ctx, group); err != nil {
		return identity, false, fmt.Errorf("create dedicated worker group: %w", err)
	}
	groupCreated := true
	defer func() {
		if resultErr != nil && groupCreated {
			resultErr = errors.Join(resultErr, accounts.store.DeleteGroup(context.Background(), group.Name))
		}
	}()
	if err := accounts.store.CreateUser(ctx, user); err != nil {
		return identity, false, fmt.Errorf("create dedicated worker user: %w", err)
	}
	userCreated := true
	defer func() {
		if resultErr != nil && userCreated {
			resultErr = errors.Join(resultErr, accounts.store.DeleteUser(context.Background(), user.Name))
		}
	}()
	identity, err = accounts.Resolve(ctx)
	if err != nil {
		return identity, false, fmt.Errorf("validate created worker identity: %w", err)
	}
	groupCreated = false
	userCreated = false
	return identity, true, nil
}

func (accounts *dedicatedAccounts) Purge(ctx context.Context) (resultErr error) {
	if accounts == nil || accounts.store == nil {
		return errors.New("worker account store is required")
	}
	user, userExists, err := accounts.store.LookupUser(ctx, privsep.ProductionUser)
	if err != nil {
		return err
	}
	group, groupExists, err := accounts.store.LookupGroup(ctx, privsep.ProductionGroup)
	if err != nil {
		return err
	}
	if !userExists && !groupExists {
		return nil
	}
	if !userExists || !groupExists {
		return fmt.Errorf("refuse to purge incomplete dedicated worker identity: user=%t group=%t", userExists, groupExists)
	}
	if _, err := validateDedicatedAccount(user, group); err != nil {
		return fmt.Errorf("refuse to purge unrecognized worker identity: %w", err)
	}
	if err := accounts.store.DeleteUser(ctx, user.Name); err != nil {
		return fmt.Errorf("delete dedicated worker user: %w", err)
	}
	if err := accounts.store.DeleteGroup(ctx, group.Name); err != nil {
		return errors.Join(fmt.Errorf("delete dedicated worker group: %w", err), accounts.store.CreateUser(context.Background(), user))
	}
	return nil
}

func (accounts *dedicatedAccounts) Restore(ctx context.Context, identity privsep.Identity) (resultErr error) {
	if accounts == nil || accounts.store == nil {
		return errors.New("worker account store is required")
	}
	user, group, err := accountRecordsForIdentity(identity)
	if err != nil {
		return err
	}
	existingUser, userExists, err := accounts.store.LookupUser(ctx, user.Name)
	if err != nil {
		return err
	}
	existingGroup, groupExists, err := accounts.store.LookupGroup(ctx, group.Name)
	if err != nil {
		return err
	}
	if userExists || groupExists {
		if userExists && groupExists {
			_, validationErr := validateDedicatedAccount(existingUser, existingGroup)
			if validationErr == nil && existingUser.UID == identity.UID && existingGroup.GID == identity.GID {
				return nil
			}
		}
		return fmt.Errorf("refuse to restore over existing worker identity: user=%t group=%t", userExists, groupExists)
	}
	if err := accounts.store.CreateGroup(ctx, group); err != nil {
		return fmt.Errorf("restore dedicated worker group: %w", err)
	}
	groupCreated := true
	defer func() {
		if resultErr != nil && groupCreated {
			resultErr = errors.Join(resultErr, accounts.store.DeleteGroup(context.Background(), group.Name))
		}
	}()
	if err := accounts.store.CreateUser(ctx, user); err != nil {
		return fmt.Errorf("restore dedicated worker user: %w", err)
	}
	groupCreated = false
	return nil
}

func accountRecordsForIdentity(identity privsep.Identity) (accountUser, accountGroup, error) {
	user := accountUser{
		Name: identity.User, UID: identity.UID, GID: identity.GID, Home: identity.Home,
		Shell: workerAccountShell, Password: workerPasswordMarker, Comment: workerAccountMarker,
	}
	group := accountGroup{
		Name: identity.Group, GID: identity.GID, Password: workerPasswordMarker, Comment: workerAccountMarker,
	}
	if _, err := validateDedicatedAccount(user, group); err != nil {
		return accountUser{}, accountGroup{}, fmt.Errorf("invalid worker identity restoration: %w", err)
	}
	return user, group, nil
}

func validateDedicatedAccount(user accountUser, group accountGroup) (privsep.Identity, error) {
	identity := privsep.Identity{
		User: user.Name, Group: group.Name, UID: user.UID, GID: user.GID, Home: user.Home,
	}
	var failures []error
	if user.Name != privsep.ProductionUser {
		failures = append(failures, fmt.Errorf("worker user=%q, want %q", user.Name, privsep.ProductionUser))
	}
	if group.Name != privsep.ProductionGroup {
		failures = append(failures, fmt.Errorf("worker group=%q, want %q", group.Name, privsep.ProductionGroup))
	}
	if user.UID < workerIDMinimum || user.UID > workerIDMaximum || user.GID == 0 {
		failures = append(failures, fmt.Errorf("worker uid=%d gid=%d is outside the dedicated system range", user.UID, user.GID))
	}
	if user.GID != group.GID {
		failures = append(failures, fmt.Errorf("worker primary GID=%d does not match group GID=%d", user.GID, group.GID))
	}
	if user.Home != privsep.ProductionHome {
		failures = append(failures, fmt.Errorf("worker home=%q, want %q", user.Home, privsep.ProductionHome))
	}
	if user.Shell != workerAccountShell {
		failures = append(failures, fmt.Errorf("worker shell=%q, want %q", user.Shell, workerAccountShell))
	}
	if user.Password != workerPasswordMarker || group.Password != workerPasswordMarker {
		failures = append(failures, errors.New("worker user and group must have disabled password markers"))
	}
	if user.Comment != workerAccountMarker || group.Comment != workerAccountMarker {
		failures = append(failures, errors.New("worker user and group lack the tun-proxy ownership marker"))
	}
	if err := errors.Join(failures...); err != nil {
		return privsep.Identity{}, err
	}
	return identity, nil
}

func allocateWorkerID(used map[uint32]struct{}) (uint32, error) {
	for candidate := workerIDMaximum; candidate >= workerIDMinimum; candidate-- {
		if _, exists := used[candidate]; !exists {
			return candidate, nil
		}
	}
	return 0, fmt.Errorf("no unused worker UID/GID in range %d-%d", workerIDMinimum, workerIDMaximum)
}

type dsclStore struct{ runner CommandRunner }

func (store dsclStore) LookupUser(ctx context.Context, name string) (accountUser, bool, error) {
	values, exists, err := store.lookup(ctx, "/Users", name, []string{
		"RecordName", "UniqueID", "PrimaryGroupID", "NFSHomeDirectory", "UserShell", "Password", "Comment",
	})
	if err != nil || !exists {
		return accountUser{}, exists, err
	}
	uid, err := accountID("UniqueID", values["UniqueID"])
	if err != nil {
		return accountUser{}, false, err
	}
	gid, err := accountID("PrimaryGroupID", values["PrimaryGroupID"])
	if err != nil {
		return accountUser{}, false, err
	}
	return accountUser{
		Name: values["RecordName"], UID: uid, GID: gid, Home: values["NFSHomeDirectory"],
		Shell: values["UserShell"], Password: values["Password"], Comment: values["Comment"],
	}, true, nil
}

func (store dsclStore) LookupGroup(ctx context.Context, name string) (accountGroup, bool, error) {
	values, exists, err := store.lookup(ctx, "/Groups", name, []string{"RecordName", "PrimaryGroupID", "Password", "Comment"})
	if err != nil || !exists {
		return accountGroup{}, exists, err
	}
	gid, err := accountID("PrimaryGroupID", values["PrimaryGroupID"])
	if err != nil {
		return accountGroup{}, false, err
	}
	return accountGroup{Name: values["RecordName"], GID: gid, Password: values["Password"], Comment: values["Comment"]}, true, nil
}

func (store dsclStore) UsedIDs(ctx context.Context) (map[uint32]struct{}, error) {
	if store.runner == nil {
		return nil, errors.New("directory service command runner is required")
	}
	used := make(map[uint32]struct{})
	for _, query := range [][2]string{{"/Users", "UniqueID"}, {"/Groups", "PrimaryGroupID"}} {
		output, err := store.runner.Run(ctx, dsclPath, ".", "-list", query[0], query[1])
		if err != nil {
			return nil, fmt.Errorf("list directory service IDs for %s: %w", query[0], err)
		}
		for _, line := range strings.Split(string(output), "\n") {
			fields := strings.Fields(line)
			if len(fields) < 2 {
				continue
			}
			value, err := strconv.ParseInt(fields[len(fields)-1], 10, 64)
			if err == nil && value >= 0 && value <= int64(^uint32(0)) {
				used[uint32(value)] = struct{}{}
			}
		}
	}
	return used, nil
}

func (store dsclStore) CreateGroup(ctx context.Context, group accountGroup) error {
	return store.create(ctx, "/Groups/"+group.Name, [][2]string{
		{"RecordName", group.Name}, {"PrimaryGroupID", strconv.FormatUint(uint64(group.GID), 10)},
		{"Password", group.Password}, {"RealName", workerAccountName}, {"Comment", group.Comment},
	})
}

func (store dsclStore) CreateUser(ctx context.Context, user accountUser) error {
	return store.create(ctx, "/Users/"+user.Name, [][2]string{
		{"RecordName", user.Name}, {"UniqueID", strconv.FormatUint(uint64(user.UID), 10)},
		{"PrimaryGroupID", strconv.FormatUint(uint64(user.GID), 10)}, {"NFSHomeDirectory", user.Home},
		{"UserShell", user.Shell}, {"Password", user.Password}, {"RealName", workerAccountName}, {"Comment", user.Comment},
	})
}

func (store dsclStore) DeleteUser(ctx context.Context, name string) error {
	return store.delete(ctx, "/Users/"+name)
}

func (store dsclStore) DeleteGroup(ctx context.Context, name string) error {
	return store.delete(ctx, "/Groups/"+name)
}

func (store dsclStore) lookup(ctx context.Context, collection, name string, attributes []string) (map[string]string, bool, error) {
	if store.runner == nil {
		return nil, false, errors.New("directory service command runner is required")
	}
	output, err := store.runner.Run(ctx, dsclPath, ".", "-search", collection, "RecordName", name)
	if err != nil {
		return nil, false, fmt.Errorf("search directory service record %s/%s: %w", collection, name, err)
	}
	exists := false
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 && fields[0] == name {
			exists = true
			break
		}
	}
	if !exists {
		return nil, false, nil
	}
	args := []string{".", "-read", collection + "/" + name}
	args = append(args, attributes...)
	output, err = store.runner.Run(ctx, dsclPath, args...)
	if err != nil {
		return nil, false, fmt.Errorf("read directory service record %s/%s: %w", collection, name, err)
	}
	values := make(map[string]string, len(attributes))
	for _, line := range strings.Split(string(output), "\n") {
		key, value, found := strings.Cut(line, ":")
		if found {
			values[strings.TrimSpace(key)] = strings.TrimSpace(value)
		}
	}
	for _, attribute := range attributes {
		if values[attribute] == "" {
			return nil, false, fmt.Errorf("directory service record %s/%s lacks %s", collection, name, attribute)
		}
	}
	return values, true, nil
}

func (store dsclStore) create(ctx context.Context, path string, attributes [][2]string) (resultErr error) {
	if store.runner == nil {
		return errors.New("directory service command runner is required")
	}
	if _, err := store.runner.Run(ctx, dsclPath, ".", "-create", path); err != nil {
		return err
	}
	created := true
	defer func() {
		if resultErr != nil && created {
			_, cleanupErr := store.runner.Run(context.Background(), dsclPath, ".", "-delete", path)
			resultErr = errors.Join(resultErr, cleanupErr)
		}
	}()
	for _, attribute := range attributes {
		if _, err := store.runner.Run(ctx, dsclPath, ".", "-create", path, attribute[0], attribute[1]); err != nil {
			return err
		}
	}
	created = false
	return nil
}

func (store dsclStore) delete(ctx context.Context, path string) error {
	if store.runner == nil {
		return errors.New("directory service command runner is required")
	}
	_, err := store.runner.Run(ctx, dsclPath, ".", "-delete", path)
	return err
}

func accountID(name, raw string) (uint32, error) {
	value, err := strconv.ParseUint(raw, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("invalid directory service %s %q: %w", name, raw, err)
	}
	return uint32(value), nil
}
