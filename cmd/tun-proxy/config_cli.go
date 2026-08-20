package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/hailinpan/tun-proxy/internal/app"
	"github.com/hailinpan/tun-proxy/internal/config"
	"github.com/hailinpan/tun-proxy/internal/defaultconfig"
	"github.com/hailinpan/tun-proxy/internal/launchservice"
)

type configValidationResult struct {
	Valid          bool   `json:"valid"`
	Path           string `json:"path"`
	Digest         string `json:"digest"`
	Summary        string `json:"summary"`
	ManagedService bool   `json:"managed_service"`
}

type finderCommandRunner func(context.Context, string, ...string) ([]byte, error)

func configCommand(args []string) error {
	if len(args) == 0 {
		return usageError([]string{"config"}, "a config command is required")
	}
	switch args[0] {
	case "validate":
		return configValidateCommand(args[1:])
	case "help", "-h", "--help":
		return helpCommand(append([]string{"config"}, args[1:]...))
	default:
		if strings.HasPrefix(args[0], "-") {
			return configOptionsCommand(context.Background(), args, executeFinderCommand)
		}
		return usageError([]string{"config"}, fmt.Sprintf("unknown config command %q", args[0]))
	}
}

func configOptionsCommand(ctx context.Context, args []string, runner finderCommandRunner) error {
	flags := flag.NewFlagSet("config", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	flags.Usage = func() { fprintUsage(flags.Output(), []string{"config"}) }
	finder := flags.Bool("finder", false, "reveal the selected configuration in Finder")
	generate := flags.Bool("generate", false, "generate the embedded default configuration")
	force := flags.Bool("force", false, "overwrite an existing configuration with -generate")
	path := flags.String("config", defaultUserConfigPath(), "configuration file to use")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("config received unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	if *finder == *generate {
		return usageError([]string{"config"}, "choose exactly one of -finder or -generate")
	}
	if *force && !*generate {
		return usageError([]string{"config"}, "-force requires -generate")
	}
	if *generate {
		generated, err := generateDefaultConfig(*path, *force)
		if err != nil {
			return err
		}
		fmt.Printf("generated config: %s\n", generated)
		return nil
	}
	revealed, err := revealConfigInFinder(ctx, *path, runner)
	if err != nil {
		return err
	}
	fmt.Printf("revealed config in Finder: %s\n", revealed)
	return nil
}

func generateDefaultConfig(path string, force bool) (string, error) {
	contents := defaultconfig.Bytes()
	if _, _, err := config.LoadBytesWithDigest(contents); err != nil {
		return "", fmt.Errorf("validate embedded default configuration: %w", err)
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve config path %q: %w", path, err)
	}
	absolute = filepath.Clean(absolute)
	directory := filepath.Dir(absolute)
	directoryInfo, err := os.Lstat(directory)
	directoryCreated := false
	if errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return "", fmt.Errorf("create config directory %q: %w", directory, err)
		}
		directoryCreated = true
		directoryInfo, err = os.Lstat(directory)
	}
	if err != nil {
		return "", fmt.Errorf("inspect config directory %q: %w", directory, err)
	}
	resolvedDirectory, err := os.Stat(directory)
	if err != nil {
		return "", fmt.Errorf("resolve config directory %q: %w", directory, err)
	}
	if !resolvedDirectory.IsDir() {
		return "", fmt.Errorf("config directory %q is not a directory", directory)
	}
	defaultPath, defaultPathErr := filepath.Abs(defaultUserConfigPath())
	secureDirectory := directoryCreated || (defaultPathErr == nil &&
		directoryInfo.Mode()&os.ModeSymlink == 0 && absolute == filepath.Clean(defaultPath))
	if secureDirectory {
		if err := os.Chmod(directory, 0o700); err != nil {
			return "", fmt.Errorf("secure config directory %q: %w", directory, err)
		}
	}
	if target, err := os.Lstat(absolute); err == nil {
		if target.Mode()&os.ModeSymlink != 0 || !target.Mode().IsRegular() {
			return "", fmt.Errorf("config %q is not a regular file", absolute)
		}
		if !force {
			return "", fmt.Errorf("config %q already exists; use -force to overwrite it", absolute)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("inspect config %q: %w", absolute, err)
	}

	temporary, err := os.CreateTemp(directory, ".config.yaml-*")
	if err != nil {
		return "", fmt.Errorf("create temporary config in %q: %w", directory, err)
	}
	temporaryPath := temporary.Name()
	removeTemporary := true
	defer func() {
		if removeTemporary {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return "", fmt.Errorf("secure temporary config %q: %w", temporaryPath, err)
	}
	if _, err := temporary.Write(contents); err != nil {
		_ = temporary.Close()
		return "", fmt.Errorf("write temporary config %q: %w", temporaryPath, err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return "", fmt.Errorf("sync temporary config %q: %w", temporaryPath, err)
	}
	if err := temporary.Close(); err != nil {
		return "", fmt.Errorf("close temporary config %q: %w", temporaryPath, err)
	}
	if !force {
		if err := os.Link(temporaryPath, absolute); err != nil {
			if errors.Is(err, os.ErrExist) {
				return "", fmt.Errorf("config %q already exists; use -force to overwrite it", absolute)
			}
			return "", fmt.Errorf("install config %q: %w", absolute, err)
		}
		if err := os.Remove(temporaryPath); err != nil {
			return "", fmt.Errorf("remove temporary config %q: %w", temporaryPath, err)
		}
		removeTemporary = false
	} else {
		if err := os.Rename(temporaryPath, absolute); err != nil {
			return "", fmt.Errorf("replace config %q: %w", absolute, err)
		}
		removeTemporary = false
	}
	return absolute, nil
}

func revealConfigInFinder(ctx context.Context, path string, runner finderCommandRunner) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve config path %q: %w", path, err)
	}
	absolute = filepath.Clean(absolute)
	info, err := os.Stat(absolute)
	if err != nil {
		return "", fmt.Errorf("inspect config %q: %w", absolute, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("config %q is not a regular file", absolute)
	}
	output, err := runner(ctx, "/usr/bin/open", "-R", absolute)
	if err != nil {
		message := strings.TrimSpace(string(output))
		if message == "" {
			return "", fmt.Errorf("reveal config %q in Finder: %w", absolute, err)
		}
		return "", fmt.Errorf("reveal config %q in Finder: %w: %s", absolute, err, message)
	}
	return absolute, nil
}

func executeFinderCommand(ctx context.Context, executable string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, executable, args...).CombinedOutput()
}

func configValidateCommand(args []string) error {
	flags := flag.NewFlagSet("config validate", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	flags.Usage = func() { fprintUsage(flags.Output(), []string{"config", "validate"}) }
	path := flags.String("config", defaultUserConfigPath(), "path to YAML configuration")
	managed := flags.Bool("service", false, "enforce the managed service path contract")
	jsonOutput := flags.Bool("json", false, "print validation result as JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("config validate received unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	result, err := validateConfigFile(*path, *managed, launchservice.DefaultLayout())
	if err != nil {
		return err
	}
	if *jsonOutput {
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(result)
	}
	if result.Summary == "" {
		return errors.New("configuration summary is empty")
	}
	fmt.Printf("%s digest=%s\n", result.Summary, result.Digest)
	return nil
}

func validateConfigFile(path string, managed bool, layout launchservice.Layout) (configValidationResult, error) {
	runtime, digest, err := config.LoadFileWithDigest(path)
	if err != nil {
		return configValidationResult{}, err
	}
	if managed {
		if err := launchservice.ValidateManagedConfig(runtime, layout); err != nil {
			return configValidationResult{}, fmt.Errorf("managed service configuration: %w", err)
		}
	}
	return configValidationResult{
		Valid:          true,
		Path:           path,
		Digest:         digest,
		Summary:        app.Summary(runtime),
		ManagedService: managed,
	}, nil
}
