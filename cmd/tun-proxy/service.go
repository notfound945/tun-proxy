package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/hailinpan/tun-proxy/internal/app"
	"github.com/hailinpan/tun-proxy/internal/apperror"
	"github.com/hailinpan/tun-proxy/internal/config"
	"github.com/hailinpan/tun-proxy/internal/control"
	"github.com/hailinpan/tun-proxy/internal/launchservice"
	"github.com/hailinpan/tun-proxy/internal/privsep"
	"github.com/hailinpan/tun-proxy/internal/system"
	"golang.org/x/sys/unix"
)

func serviceCommand(args []string) (resultErr error) {
	if len(args) == 0 {
		printServiceUsage()
		return errors.New("a service command is required")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, unix.SIGTERM)
	defer stop()
	manager := newServiceManager()
	if kind, ok := serviceOperationKind(args[0]); ok && !hasOnlyHelpArgument(args[1:]) {
		guard, err := manager.BeginOperation(ctx, launchservice.OperationSpec{Kind: kind})
		if err != nil {
			return err
		}
		ctx = context.WithValue(ctx, serviceOperationIDContextKey{}, guard.Metadata.ID)
		defer func() {
			resultErr = errors.Join(resultErr, guard.Close())
		}()
	}
	switch args[0] {
	case "install":
		return serviceInstallCommand(ctx, manager, args[1:])
	case "start":
		return serviceStartCommand(ctx, manager, args[1:])
	case "stop":
		return serviceStopCommand(ctx, manager, args[1:])
	case "restart":
		if hasOnlyHelpArgument(args[1:]) {
			return helpCommand([]string{"service", "restart"})
		}
		if len(args) != 1 {
			return errors.New("service restart does not accept arguments")
		}
		if err := manager.Restart(ctx); err != nil {
			return err
		}
		fmt.Println("tun-proxy service restarted")
		return nil
	case "reload":
		return serviceReloadCommand(ctx, manager, args[1:])
	case "status":
		return serviceStatusCommand(ctx, manager, args[1:])
	case "logs":
		return serviceLogsCommand(ctx, manager.Layout, args[1:])
	case "upgrade":
		return serviceUpgradeCommand(ctx, manager, args[1:])
	case "uninstall":
		return serviceUninstallCommand(ctx, manager, args[1:])
	case "help", "-h", "--help":
		return helpCommand(append([]string{"service"}, args[1:]...))
	default:
		printServiceUsage()
		return fmt.Errorf("unknown service command %q", args[0])
	}
}

func serviceOperationKind(command string) (launchservice.OperationKind, bool) {
	switch command {
	case "install":
		return launchservice.OperationInstall, true
	case "start":
		return launchservice.OperationStart, true
	case "stop":
		return launchservice.OperationStop, true
	case "restart":
		return launchservice.OperationRestart, true
	case "reload":
		return launchservice.OperationReload, true
	case "upgrade":
		return launchservice.OperationUpgrade, true
	case "uninstall":
		return launchservice.OperationUninstall, true
	default:
		return "", false
	}
}

func hasOnlyHelpArgument(args []string) bool {
	return len(args) == 1 && (args[0] == "-h" || args[0] == "--help" || args[0] == "help")
}

type serviceStarter interface {
	Start(context.Context) error
}

type serviceStartOptions struct {
	useLastConfig bool
}

func newServiceStartFlagSet(output io.Writer, options *serviceStartOptions) *flag.FlagSet {
	if options == nil {
		options = &serviceStartOptions{}
	}
	flags := newCommandFlagSet("service start", output)
	flags.BoolVar(&options.useLastConfig, "use-last-config", false, "use the last successfully synchronized managed configuration if user synchronization fails")
	return flags
}

type serviceConfigSynchronizer interface {
	SynchronizeConfig(context.Context, []byte) (launchservice.ConfigSyncResult, error)
}

// serviceStartConfigManager is the subset of the managed-service manager used
// by service start. Keeping the synchronization step here makes start the
// single normal entry point for applying the invoking user's configuration.
type serviceStartConfigManager interface {
	serviceStarter
	serviceConfigSynchronizer
}

func serviceStartWithConfigCommand(
	ctx context.Context,
	manager serviceStartConfigManager,
	managedConfigPath string,
	args []string,
) error {
	options := serviceStartOptions{}
	flags := newServiceStartFlagSet(os.Stderr, &options)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("service start received unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}

	var previous []byte
	var previousErr error
	if options.useLastConfig {
		_, previous, _, _, previousErr = loadValidatedConfigSource(managedConfigPath)
	}

	source := defaultUserConfigPath()
	_, contents, _, _, err := loadValidatedConfigSource(source)
	if err != nil {
		return finishServiceStartFailure(
			ctx,
			manager,
			previous,
			previousErr,
			false,
			fmt.Errorf("validate user configuration before service start: %w", err),
			options.useLastConfig,
		)
	}

	syncResult, err := manager.SynchronizeConfig(ctx, contents)
	if err != nil {
		return finishServiceStartFailure(
			ctx,
			manager,
			previous,
			previousErr,
			true,
			fmt.Errorf("synchronize user configuration before service start: %w", err),
			options.useLastConfig,
		)
	}
	if syncResult.Restarted {
		fmt.Printf("tun-proxy service started with synchronized user configuration: %s\n", source)
		return nil
	}
	if err := manager.Start(ctx); err != nil {
		return finishServiceStartFailure(
			ctx,
			manager,
			previous,
			previousErr,
			true,
			fmt.Errorf("start service with synchronized user configuration: %w", err),
			options.useLastConfig,
		)
	}
	fmt.Printf("tun-proxy service started with synchronized user configuration: %s\n", source)
	return nil
}

func finishServiceStartFailure(
	ctx context.Context,
	manager serviceStartConfigManager,
	previous []byte,
	previousErr error,
	restorePrevious bool,
	cause error,
	useLastConfig bool,
) error {
	if !useLastConfig {
		return warnServiceStartFailure(cause)
	}
	if previousErr != nil {
		return warnServiceStartFailure(errors.Join(cause, fmt.Errorf("validate last successfully synchronized managed configuration: %w", previousErr)))
	}
	if len(previous) == 0 {
		return warnServiceStartFailure(errors.Join(cause, errors.New("last successfully synchronized managed configuration is empty")))
	}
	if restorePrevious {
		if result, err := manager.SynchronizeConfig(ctx, previous); err != nil {
			return warnServiceStartFailure(errors.Join(cause, fmt.Errorf("restore last successfully synchronized managed configuration: %w", err)))
		} else if result.Restarted {
			fmt.Fprintln(os.Stderr, "WARN user configuration could not be synchronized; continuing with the last successfully synchronized managed configuration")
			fmt.Println("tun-proxy service started with the last successfully synchronized managed configuration")
			return nil
		}
	}
	if err := manager.Start(ctx); err != nil {
		return warnServiceStartFailure(errors.Join(cause, fmt.Errorf("start service with last successfully synchronized managed configuration: %w", err)))
	}
	fmt.Fprintln(os.Stderr, "WARN user configuration could not be synchronized; continuing with the last successfully synchronized managed configuration")
	fmt.Println("tun-proxy service started with the last successfully synchronized managed configuration")
	return nil
}

func warnServiceStartFailure(err error) error {
	if err == nil {
		return nil
	}
	fmt.Fprintf(os.Stderr, "WARN service start aborted: %v\n", err)
	return withServiceLogsHint(err)
}

func serviceStartCommand(ctx context.Context, starter serviceStarter, args []string) error {
	if hasOnlyHelpArgument(args) {
		return helpCommand([]string{"service", "start"})
	}
	if manager, ok := starter.(serviceStartConfigManager); ok {
		return serviceStartWithConfigCommand(ctx, manager, launchservice.DefaultLayout().Config, args)
	}
	if len(args) != 0 {
		return errors.New("service start does not accept arguments")
	}
	if err := starter.Start(ctx); err != nil {
		return withServiceLogsHint(err)
	}
	fmt.Println("tun-proxy service started")
	return nil
}

type serviceStopper interface {
	Stop(context.Context) error
}

func serviceStopCommand(ctx context.Context, stopper serviceStopper, args []string) error {
	if hasOnlyHelpArgument(args) {
		return helpCommand([]string{"service", "stop"})
	}
	if len(args) != 0 {
		return errors.New("service stop does not accept arguments")
	}
	if err := stopper.Stop(ctx); err != nil {
		return withServiceLogsHint(err)
	}
	fmt.Println("tun-proxy service stopped and unloaded")
	return nil
}

type serviceReloadOptions struct {
	timeout       time.Duration
	configPath    string
	useUserConfig bool
}

func newServiceReloadFlagSet(output io.Writer, options *serviceReloadOptions) *flag.FlagSet {
	if options == nil {
		options = &serviceReloadOptions{}
	}
	flags := newCommandFlagSet("service reload", output)
	flags.DurationVar(&options.timeout, "timeout", 15*time.Second, "wait `DURATION` for the final supervisor/worker result")
	flags.StringVar(&options.configPath, "config", "", "validate and install configuration `PATH` before reload")
	flags.BoolVar(&options.useUserConfig, "user-config", false, "reload the invoking user's default configuration")
	return flags
}

func serviceReloadCommand(ctx context.Context, manager *launchservice.Manager, args []string) (resultErr error) {
	options := serviceReloadOptions{}
	flags := newServiceReloadFlagSet(os.Stderr, &options)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("service reload received unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	if options.timeout <= 0 {
		return apperror.New(apperror.CodeUsageError, "service.reload", "service reload timeout must be positive")
	}
	configPath, err := resolveServiceReloadConfigPath(options)
	if err != nil {
		return err
	}
	defer func() {
		resultErr = withServiceLogsHint(resultErr)
	}()

	status, err := manager.Status(ctx)
	if err != nil {
		return fmt.Errorf("inspect service before reload: %w", err)
	}
	if err := validateServiceReloadStatus(status); err != nil {
		return err
	}

	var configContents []byte
	var expectedDigest string
	if configPath != "" {
		_, contents, next, digest, err := loadValidatedConfigSource(configPath)
		if err != nil {
			return fmt.Errorf("validate reload configuration: %w", err)
		}
		current, err := config.LoadFile(manager.Layout.Config)
		if err != nil {
			return apperror.Wrap(apperror.CodeConfigInvalid, "service.reload", "managed configuration is invalid", err)
		}
		if err := app.PreflightReload(ctx, current, next); err != nil {
			return fmt.Errorf("validate configuration for live reload: %w", err)
		}
		configContents = contents
		expectedDigest = digest
	} else {
		_, _, _, expectedDigest, err = loadValidatedConfigSource(manager.Layout.Config)
		if err != nil {
			return fmt.Errorf("validate managed configuration before reload: %w", err)
		}
	}

	state, err := system.ReadState(manager.Layout.State)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return serviceReloadNotRunningError("")
		}
		return fmt.Errorf("read service state before reload: %w", err)
	}
	if state.ControlSocket == "" {
		return apperror.New(apperror.CodeServiceProtocolTooOld, "service.reload", "running service does not advertise a reload control socket")
	}
	if state.ControlSocket != manager.Layout.ControlSocket {
		return apperror.Wrap(
			apperror.CodeUnsafeFile,
			"service.reload",
			"service state references an unexpected control socket",
			fmt.Errorf("got %q, want %q", state.ControlSocket, manager.Layout.ControlSocket),
		).WithDetails(map[string]any{"socket_path": state.ControlSocket})
	}

	request, configUpdate, err := prepareServiceReload(ctx, manager, configContents, expectedDigest, newServiceReloadRequest)
	if err != nil {
		return err
	}
	response, reloadErr := requestServiceReload(ctx, state.ControlSocket, uint32(manager.OwnerUID), request, options.timeout, control.Reload)
	if reloadErr != nil {
		if configUpdate == nil {
			return reloadErr
		}
		rollbackErr := rollbackServiceReloadConfig(ctx, manager, configUpdate, state.ControlSocket, options.timeout, request.OperationID, request.RequestID, control.Reload)
		if rollbackErr != nil {
			applyCause := apperror.Wrap(
				apperror.CodeOf(reloadErr),
				"service.reload",
				"synchronized configuration reload failed",
				reloadErr,
			)
			rollbackCause := apperror.Wrap(
				apperror.CodeOf(rollbackErr),
				"service.reload",
				"failed to restore the rolled-back configuration in the running service",
				rollbackErr,
			)
			return apperror.Wrap(
				apperror.CodeRollbackIncomplete,
				"service.reload",
				"configuration reload failed and rollback was incomplete",
				errors.Join(applyCause, rollbackCause),
			).WithDetails(serviceReloadDetails(request, "rollback"))
		}
		return apperror.Wrap(
			apperror.CodeOf(reloadErr),
			"service.reload",
			"configuration reload failed; managed configuration was rolled back",
			reloadErr,
		).WithDetails(serviceReloadDetails(request, "rollback"))
	}
	if configUpdate != nil {
		if err := configUpdate.Commit(); err != nil {
			return fmt.Errorf("configuration reloaded but managed configuration commit cleanup failed: %w", err)
		}
		fmt.Printf("managed configuration synchronized: %s\n", manager.Layout.Config)
	}
	fmt.Printf("tun-proxy service reloaded config=%s\n", response.ConfigDigest)
	return nil
}

func resolveServiceReloadConfigPath(options serviceReloadOptions) (string, error) {
	if options.useUserConfig && options.configPath != "" {
		return "", apperror.New(apperror.CodeUsageError, "service.reload", "service reload -user-config and -config cannot be used together")
	}
	if options.useUserConfig {
		return defaultUserConfigPath(), nil
	}
	return options.configPath, nil
}

type serviceOperationIDContextKey struct{}

type serviceControlReload func(context.Context, string, uint32, control.ReloadRequest) (control.ReloadResponse, error)
type serviceReloadRequestFactory func(context.Context, string, string) (control.ReloadRequest, error)

type serviceConfigUpdateBeginner interface {
	BeginConfigUpdate([]byte) (*launchservice.ConfigUpdate, error)
}

func prepareServiceReload(
	ctx context.Context,
	updater serviceConfigUpdateBeginner,
	configContents []byte,
	expectedDigest string,
	newRequest serviceReloadRequestFactory,
) (control.ReloadRequest, *launchservice.ConfigUpdate, error) {
	request, err := newRequest(ctx, expectedDigest, "")
	if err != nil {
		return control.ReloadRequest{}, nil, err
	}
	if configContents == nil {
		return request, nil, nil
	}
	configUpdate, err := updater.BeginConfigUpdate(configContents)
	if err != nil {
		return control.ReloadRequest{}, nil, fmt.Errorf("synchronize managed configuration: %w", err)
	}
	return request, configUpdate, nil
}

func newServiceReloadRequest(ctx context.Context, expectedDigest, rollbackOf string) (control.ReloadRequest, error) {
	operationID, _ := ctx.Value(serviceOperationIDContextKey{}).(string)
	if operationID == "" {
		var err error
		operationID, err = control.NewRequestID()
		if err != nil {
			return control.ReloadRequest{}, fmt.Errorf("generate service operation ID: %w", err)
		}
	}
	requestID, err := control.NewRequestID()
	if err != nil {
		return control.ReloadRequest{}, fmt.Errorf("generate reload request ID: %w", err)
	}
	return control.ReloadRequest{
		Version: control.Version, Kind: control.KindReload, RequestID: requestID,
		OperationID: operationID, RollbackOf: rollbackOf, ExpectedConfigDigest: expectedDigest,
	}, nil
}

func requestServiceReload(
	ctx context.Context,
	socket string,
	serverUID uint32,
	request control.ReloadRequest,
	timeout time.Duration,
	reload serviceControlReload,
) (control.ReloadResponse, error) {
	if reload == nil {
		return control.ReloadResponse{}, apperror.New(apperror.CodeInternalError, "service.reload", "service control reload client is unavailable")
	}
	reloadCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		response, err := reload(reloadCtx, socket, serverUID, request)
		if err == nil && response.Result == control.ResultSucceeded {
			if response.ConfigDigest != request.ExpectedConfigDigest {
				return response, apperror.Wrap(
					apperror.CodeReloadDigestMismatch,
					"service.reload",
					"supervisor activated an unexpected configuration digest",
					fmt.Errorf("got %q, want %q", response.ConfigDigest, request.ExpectedConfigDigest),
				).WithDetails(serviceReloadDigestDetails(request, response.ConfigDigest))
			}
			return response, nil
		}
		if err != nil && !control.IsTransportError(err) {
			return response, annotateServiceReloadError(err, request, "request")
		}
		if err == nil && response.Result != control.ResultRunning {
			return response, apperror.Wrap(
				apperror.CodeServiceProtocolTooOld,
				"service.reload",
				"supervisor returned an unsupported reload result",
				fmt.Errorf("result %q", response.Result),
			).WithDetails(serviceReloadDetails(request, "request"))
		}
		timer := time.NewTimer(50 * time.Millisecond)
		select {
		case <-reloadCtx.Done():
			timer.Stop()
			return response, apperror.Wrap(
				apperror.CodeReloadTimeout,
				"service.reload",
				"configuration reload did not complete before the timeout",
				reloadCtx.Err(),
			).WithDetails(serviceReloadDetails(request, "request"))
		case <-timer.C:
		}
	}
}

func serviceReloadDetails(request control.ReloadRequest, phase string) map[string]any {
	details := map[string]any{}
	if request.OperationID != "" {
		details["operation_id"] = request.OperationID
	}
	if request.RequestID != "" {
		details["reload_request_id"] = request.RequestID
	}
	if request.RollbackOf != "" {
		details["rollback_of"] = request.RollbackOf
	}
	if phase != "" {
		details["phase"] = phase
	}
	return details
}

func serviceReloadDigestDetails(request control.ReloadRequest, actualDigest string) map[string]any {
	details := serviceReloadDetails(request, "verify")
	details["expected_config_digest"] = request.ExpectedConfigDigest
	details["actual_config_digest"] = actualDigest
	return details
}

func annotateServiceReloadError(err error, request control.ReloadRequest, phase string) error {
	if err == nil {
		return nil
	}
	code := apperror.CodeOf(err)
	var typed *apperror.Error
	isTyped := errors.As(err, &typed) && typed != nil
	if errors.Is(err, context.DeadlineExceeded) {
		code = apperror.CodeReloadTimeout
	} else if !isTyped {
		code = apperror.CodeReloadRejected
	}
	wrapped := apperror.Wrap(code, "service.reload", "supervisor rejected configuration reload", err)
	return wrapped.WithDetails(serviceReloadDetails(request, phase))
}

type serviceConfigRollback interface {
	Rollback() error
}

func rollbackServiceReloadConfig(
	ctx context.Context,
	manager *launchservice.Manager,
	update serviceConfigRollback,
	socket string,
	timeout time.Duration,
	operationID string,
	rollbackOf string,
	reload serviceControlReload,
) error {
	if err := update.Rollback(); err != nil {
		return fmt.Errorf("roll back managed configuration: %w", err)
	}
	_, _, _, rollbackDigest, err := loadValidatedConfigSource(manager.Layout.Config)
	if err != nil {
		return fmt.Errorf("validate rolled-back managed configuration: %w", err)
	}
	recoveryCtx, cancel := context.WithTimeout(context.Background(), timeout+5*time.Second)
	defer cancel()
	recoveryCtx = context.WithValue(recoveryCtx, serviceOperationIDContextKey{}, operationID)
	request, err := newServiceReloadRequest(recoveryCtx, rollbackDigest, rollbackOf)
	if err != nil {
		return err
	}
	if _, err := requestServiceReload(recoveryCtx, socket, uint32(manager.OwnerUID), request, timeout, reload); err != nil {
		return fmt.Errorf("restore rolled-back configuration in runtime: %w", err)
	}
	return nil
}

func validateServiceReloadStatus(status launchservice.Status) error {
	if !status.Installed {
		return apperror.New(
			apperror.CodeServiceNotInstalled,
			"service.reload",
			fmt.Sprintf("tun-proxy service is not installed; run %q first", launchservice.InstallCommand),
		)
	}
	if status.Runtime.Running && status.Runtime.Phase == "running" {
		return nil
	}
	return serviceReloadNotRunningError(status.Runtime.Phase)
}

func serviceReloadNotRunningError(phase string) error {
	return apperror.New(
		apperror.CodeServiceNotRunning,
		"service.reload",
		fmt.Sprintf("tun-proxy service is not running (phase=%q); run %q first", phase, launchservice.StartCommand),
	).WithDetails(map[string]any{"phase": phase})
}

type serviceInstallOptions struct {
	configPath  string
	binaryPath  string
	start       bool
	startAtBoot bool
}

func newServiceInstallFlagSet(output io.Writer, options *serviceInstallOptions) *flag.FlagSet {
	if options == nil {
		options = &serviceInstallOptions{}
	}
	flags := newCommandFlagSet("service install", output)
	flags.StringVar(&options.configPath, "config", defaultUserConfigPath(), "configuration `PATH` to install")
	flags.StringVar(&options.binaryPath, "binary", "", "tun-proxy binary `PATH` to install (default: current executable)")
	flags.BoolVar(&options.start, "start", true, "start after installation")
	flags.BoolVar(&options.startAtBoot, "start-at-boot", false, "start automatically at system boot")
	return flags
}

const serviceLogsHintCommand = "sudo tun-proxy service logs"

func serviceInstallCommand(ctx context.Context, manager *launchservice.Manager, args []string) (resultErr error) {
	defer func() {
		resultErr = withServiceLogsHint(resultErr)
	}()

	options := serviceInstallOptions{}
	flags := newServiceInstallFlagSet(os.Stderr, &options)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("service install received unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	configSource, err := validateConfigSource(options.configPath)
	if err != nil {
		return err
	}
	binarySource, err := serviceBinarySource(options.binaryPath)
	if err != nil {
		return err
	}
	logCheckpoints := checkpointManagedLogs(manager.Layout)
	if err := manager.Install(ctx, binarySource, configSource, options.start, options.startAtBoot); err != nil {
		return withServiceInstallLogDiagnostics(err, manager.Layout, logCheckpoints)
	}
	if options.start {
		fmt.Println("tun-proxy service installed and started")
	} else {
		fmt.Println("tun-proxy service installed")
	}
	return nil
}

func withServiceLogsHint(err error) error {
	if err == nil || errors.Is(err, flag.ErrHelp) || strings.Contains(err.Error(), serviceLogsHintCommand) {
		return err
	}
	return fmt.Errorf("%w\nto inspect service logs, run: %s", err, serviceLogsHintCommand)
}

type serviceStatusOptions struct{ jsonOutput bool }

func newServiceStatusFlagSet(output io.Writer, options *serviceStatusOptions) *flag.FlagSet {
	if options == nil {
		options = &serviceStatusOptions{}
	}
	flags := newCommandFlagSet("service status", output)
	flags.BoolVar(&options.jsonOutput, "json", false, "print service status as JSON")
	return flags
}

func serviceStatusCommand(ctx context.Context, manager *launchservice.Manager, args []string) error {
	options := serviceStatusOptions{}
	flags := newServiceStatusFlagSet(os.Stderr, &options)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("service status received unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	status, err := manager.Status(ctx)
	if err != nil {
		return err
	}
	if options.jsonOutput {
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(status)
	}
	fmt.Printf("installed=%t loaded=%t running=%t pid=%d phase=%s\n",
		status.Installed, status.Loaded, status.Runtime.Running, status.Runtime.PID, status.Runtime.Phase)
	return nil
}

type serviceUpgradeOptions struct {
	binaryPath       string
	configPath       string
	startAtBootValue string
}

func newServiceUpgradeFlagSet(output io.Writer, options *serviceUpgradeOptions) *flag.FlagSet {
	if options == nil {
		options = &serviceUpgradeOptions{}
	}
	flags := newCommandFlagSet("service upgrade", output)
	flags.StringVar(&options.binaryPath, "binary", "", "replacement binary `PATH` (default: current executable)")
	flags.StringVar(&options.configPath, "config", "", "optional replacement configuration `PATH`")
	flags.StringVar(&options.startAtBootValue, "start-at-boot", "", "optional boot startup `BOOL` (true or false; default: preserve current)")
	return flags
}

type serviceUpgrader interface {
	Upgrade(context.Context, string, string, *bool) (launchservice.UpgradeResult, error)
}

func serviceUpgradeCommand(ctx context.Context, manager serviceUpgrader, args []string) error {
	options := serviceUpgradeOptions{}
	flags := newServiceUpgradeFlagSet(os.Stderr, &options)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("service upgrade received unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	binarySource, err := serviceBinarySource(options.binaryPath)
	if err != nil {
		return err
	}
	configSource := ""
	if options.configPath != "" {
		configSource, err = validateConfigSource(options.configPath)
		if err != nil {
			return err
		}
	}
	var startAtBoot *bool
	if options.startAtBootValue != "" {
		value, err := strconv.ParseBool(options.startAtBootValue)
		if err != nil {
			return fmt.Errorf("service upgrade start-at-boot must be true or false, got %q", options.startAtBootValue)
		}
		startAtBoot = &value
	}
	result, err := manager.Upgrade(ctx, binarySource, configSource, startAtBoot)
	if err != nil {
		return withServiceLogsHint(err)
	}
	fmt.Println(serviceUpgradeSuccessMessage(result))
	return nil
}

func serviceUpgradeSuccessMessage(result launchservice.UpgradeResult) string {
	if result.Restarted {
		return "tun-proxy service upgraded and restarted"
	}
	return "tun-proxy service upgraded; service remains stopped (startup not verified)"
}

type serviceUninstallOptions struct{ purge bool }

func newServiceUninstallFlagSet(output io.Writer, options *serviceUninstallOptions) *flag.FlagSet {
	if options == nil {
		options = &serviceUninstallOptions{}
	}
	flags := newCommandFlagSet("service uninstall", output)
	flags.BoolVar(&options.purge, "purge", false, "also remove installed config, mappings, and logs")
	return flags
}

func serviceUninstallCommand(ctx context.Context, manager *launchservice.Manager, args []string) error {
	options := serviceUninstallOptions{}
	flags := newServiceUninstallFlagSet(os.Stderr, &options)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("service uninstall received unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	if err := manager.Uninstall(ctx, options.purge); err != nil {
		return err
	}
	if options.purge {
		fmt.Println("tun-proxy service uninstalled and managed data purged")
	} else {
		fmt.Println("tun-proxy service uninstalled; config, mappings, and logs preserved")
	}
	return nil
}

func serviceRunCommand(args []string) error {
	flags := flag.NewFlagSet("_service-run", flag.ContinueOnError)
	path := flags.String("config", "", "installed service configuration")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("_service-run received unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	layout := launchservice.DefaultLayout()
	if *path != layout.Config {
		return fmt.Errorf("managed service requires -config %q", layout.Config)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, unix.SIGTERM)
	defer stop()
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, unix.SIGHUP)
	defer signal.Stop(hup)
	reload := make(chan struct{}, 1)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-hup:
				select {
				case reload <- struct{}{}:
				default:
				}
			}
		}
	}()
	return app.RunServiceSupervisor(ctx, *path, layout, app.ServiceSupervisorOptions{
		Reload: reload,
		Ready: func(tunName string) {
			writeManagedServiceLog(os.Stdout, time.Now(), fmt.Sprintf("tun-proxy service running tun=%s", tunName))
		},
		Event: func(level, message string) {
			writeManagedServiceLog(os.Stdout, time.Now(), fmt.Sprintf("%s %s", strings.ToUpper(level), message))
		},
	})
}

const managedServiceTimestampLayout = "2006-01-02T15:04:05.000-07:00"

// writeManagedServiceLog prefixes every physical log line with a local RFC 3339
// timestamp. Building the complete payload before writing also keeps multi-line
// errors from interleaving with concurrent supervisor output.
func writeManagedServiceLog(output io.Writer, now time.Time, message string) {
	timestamp := now.Local().Format(managedServiceTimestampLayout)
	message = strings.TrimSuffix(message, "\n")
	lines := strings.Split(message, "\n")
	var payload strings.Builder
	for _, line := range lines {
		fmt.Fprintf(&payload, "%s %s\n", timestamp, line)
	}
	_, _ = io.WriteString(output, payload.String())
}

func serviceWorkerCommand(args []string) error {
	if err := validateServiceWorkerInvocation(
		args,
		func() (privsep.Identity, error) {
			return privsep.ResolveProductionIdentity(privsep.SystemDirectory{})
		},
		privsep.ValidateCurrentIdentity,
	); err != nil {
		return err
	}
	resources, err := privsep.OpenInheritedResources()
	if err != nil {
		return err
	}
	defer resources.Close() //nolint:errcheck // Best-effort cleanup.
	worker, err := app.NewServiceWorker(resources)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, unix.SIGTERM)
	defer stop()
	return privsep.ServeWorker(ctx, resources.Control, worker)
}

func validateServiceWorkerInvocation(
	args []string,
	resolveIdentity func() (privsep.Identity, error),
	validateIdentity func(privsep.Identity) error,
) error {
	if len(args) != 0 {
		return fmt.Errorf("_service-worker does not accept arguments: %s", strings.Join(args, " "))
	}
	if resolveIdentity == nil || validateIdentity == nil {
		return errors.New("service worker identity validation is required")
	}
	identity, err := resolveIdentity()
	if err != nil {
		return err
	}
	if err := validateIdentity(identity); err != nil {
		return err
	}
	return nil
}

func newServiceManager() *launchservice.Manager {
	return newServiceManagerForLayout(launchservice.DefaultLayout())
}

func newServiceManagerForLayout(layout launchservice.Layout) *launchservice.Manager {
	manager := launchservice.NewManager(layout)
	manager.Recover = func(ctx context.Context) error {
		owners := []uint32{0}
		if identity, err := launchservice.ResolveWorkerIdentity(ctx); err == nil {
			owners = append(owners, identity.UID)
		}
		return app.CleanupWithStatusOwners(ctx, layout.State, layout.Lock, owners...)
	}
	return manager
}

func validateServiceRuntime(runtime *config.Config) error {
	return launchservice.ValidateManagedConfig(runtime, launchservice.DefaultLayout())
}

func validateConfigSource(path string) (string, error) {
	absolute, _, _, _, err := loadValidatedConfigSource(path)
	return absolute, err
}

func loadValidatedConfigSource(path string) (string, []byte, *config.Config, string, error) {
	const operation = "config.validate"
	invalid := func(message string, cause error) (string, []byte, *config.Config, string, error) {
		return "", nil, nil, "", apperror.Wrap(apperror.CodeConfigInvalid, operation, message, cause)
	}
	unsafe := func(message string, cause error) (string, []byte, *config.Config, string, error) {
		return "", nil, nil, "", apperror.Wrap(apperror.CodeUnsafeFile, operation, message, cause)
	}

	absolute, err := filepath.Abs(path)
	if err != nil {
		return invalid("configuration path is invalid", fmt.Errorf("resolve source path %q: %w", path, err))
	}
	absolute = filepath.Clean(absolute)
	info, err := os.Lstat(absolute)
	if err != nil {
		return invalid("configuration file cannot be inspected", fmt.Errorf("inspect source %q: %w", absolute, err))
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return unsafe("configuration source must be a regular file and not a symlink", fmt.Errorf("source %q has mode %s", absolute, info.Mode()))
	}
	file, err := os.Open(absolute)
	if err != nil {
		return invalid("configuration file cannot be opened", fmt.Errorf("open source %q: %w", absolute, err))
	}
	defer file.Close() //nolint:errcheck // Best-effort read-only source cleanup.
	opened, err := file.Stat()
	if err != nil {
		return invalid("configuration file cannot be inspected", fmt.Errorf("inspect opened source %q: %w", absolute, err))
	}
	if !os.SameFile(info, opened) {
		return unsafe("configuration source changed while opening", fmt.Errorf("source %q changed while opening", absolute))
	}
	contents, err := io.ReadAll(io.LimitReader(file, privsep.MaxConfigSize+1))
	if err != nil {
		return invalid("configuration file cannot be read", fmt.Errorf("read source %q: %w", absolute, err))
	}
	if len(contents) > privsep.MaxConfigSize {
		return invalid("configuration file is too large", fmt.Errorf("source %q exceeds %d bytes", absolute, privsep.MaxConfigSize))
	}
	runtime, digest, err := config.LoadBytesWithDigest(contents)
	if err != nil {
		return invalid("configuration is invalid", err)
	}
	if err := validateServiceRuntime(runtime); err != nil {
		return invalid("configuration is invalid for the managed service", err)
	}
	return absolute, contents, runtime, digest, nil
}

func serviceBinarySource(path string) (string, error) {
	if path == "" {
		var err error
		path, err = os.Executable()
		if err != nil {
			return "", fmt.Errorf("locate current executable: %w", err)
		}
	}
	return absoluteRegularSource(path)
}

func absoluteRegularSource(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve source path %q: %w", path, err)
	}
	absolute = filepath.Clean(absolute)
	info, err := os.Lstat(absolute)
	if err != nil {
		return "", fmt.Errorf("inspect source %q: %w", absolute, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return "", fmt.Errorf("source %q must be a regular file, not a symlink", absolute)
	}
	return absolute, nil
}

func printServiceUsage() {
	fprintUsage(os.Stderr, []string{"service"})
}
