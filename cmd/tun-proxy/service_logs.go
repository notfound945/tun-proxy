package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/hailinpan/tun-proxy/internal/launchservice"
)

const maxTailWindow = 8 << 20

type managedLog struct {
	Name string
	Path string
}

type followedLog struct {
	managedLog
	Offset int64
	Info   os.FileInfo
}

type managedLogCheckpoint struct {
	Info os.FileInfo
	Size int64
}

type managedLogCheckpoints map[string]managedLogCheckpoint

type serviceLogsOptions struct {
	lines  int
	follow bool
	clear  bool
	stream string
}

func newServiceLogsFlagSet(output io.Writer, options *serviceLogsOptions) *flag.FlagSet {
	if options == nil {
		options = &serviceLogsOptions{}
	}
	flags := newCommandFlagSet("service logs", output)
	flags.IntVar(&options.lines, "lines", 100, "number of trailing `LINES`")
	flags.IntVar(&options.lines, "n", 100, "alias for -lines")
	flags.BoolVar(&options.follow, "follow", false, "follow appended log data")
	flags.BoolVar(&options.follow, "f", false, "alias for -follow")
	flags.BoolVar(&options.clear, "clear", false, "clear selected logs before exiting or following")
	flags.StringVar(&options.stream, "stream", "both", "log `STREAM` (stdout, stderr, or both)")
	return flags
}

func serviceLogsCommand(ctx context.Context, layout launchservice.Layout, args []string) error {
	options := serviceLogsOptions{}
	flags := newServiceLogsFlagSet(os.Stderr, &options)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("service logs received unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	if options.lines < 0 || options.lines > 10000 {
		return fmt.Errorf("service logs lines must be between 0 and 10000, got %d", options.lines)
	}
	logs, err := selectedManagedLogs(layout, strings.ToLower(options.stream))
	if err != nil {
		return err
	}
	if options.clear {
		cleared, clearErr := clearManagedLogs(logs)
		for _, log := range cleared {
			fmt.Printf("cleared %s service log: %s\n", log.Name, log.Path)
		}
		if clearErr != nil {
			return clearErr
		}
		if len(cleared) == 0 {
			fmt.Println("selected service logs are already absent")
		}
		if !options.follow {
			return nil
		}
	}
	followers := make([]followedLog, 0, len(logs))
	for _, log := range logs {
		contents, info, err := tailManagedLog(log.Path, options.lines)
		if err != nil {
			if options.follow && errors.Is(err, os.ErrNotExist) {
				followers = append(followers, followedLog{managedLog: log})
				continue
			}
			return err
		}
		if len(logs) > 1 {
			fmt.Printf("==> %s (%s) <==\n", log.Name, log.Path)
		}
		if len(contents) != 0 {
			_, _ = os.Stdout.Write(contents)
			if contents[len(contents)-1] != '\n' {
				fmt.Println()
			}
		}
		followers = append(followers, followedLog{managedLog: log, Offset: info.Size(), Info: info})
	}
	if !options.follow {
		return nil
	}
	return followManagedLogs(ctx, followers, os.Stdout)
}

func selectedManagedLogs(layout launchservice.Layout, stream string) ([]managedLog, error) {
	switch stream {
	case "stdout":
		return []managedLog{{Name: "stdout", Path: layout.StandardOut}}, nil
	case "stderr":
		return []managedLog{{Name: "stderr", Path: layout.StandardErr}}, nil
	case "both":
		return []managedLog{{Name: "stdout", Path: layout.StandardOut}, {Name: "stderr", Path: layout.StandardErr}}, nil
	default:
		return nil, fmt.Errorf("service logs stream must be stdout, stderr, or both, got %q", stream)
	}
}

func clearManagedLogs(logs []managedLog) ([]managedLog, error) {
	type openedLog struct {
		managedLog
		File *os.File
	}
	opened := make([]openedLog, 0, len(logs))
	closeOpened := func() {
		for _, log := range opened {
			_ = log.File.Close()
		}
	}
	for _, log := range logs {
		file, exists, err := openManagedLogForWrite(log.Path)
		if err != nil {
			closeOpened()
			return nil, err
		}
		if exists {
			opened = append(opened, openedLog{managedLog: log, File: file})
		}
	}

	cleared := make([]managedLog, 0, len(opened))
	var failures []error
	for _, log := range opened {
		if err := log.File.Truncate(0); err != nil {
			failures = append(failures, fmt.Errorf("clear managed log %q: %w", log.Path, err))
		} else if err := log.File.Sync(); err != nil {
			failures = append(failures, fmt.Errorf("sync cleared managed log %q: %w", log.Path, err))
		} else {
			cleared = append(cleared, log.managedLog)
		}
		if err := log.File.Close(); err != nil {
			failures = append(failures, fmt.Errorf("close cleared managed log %q: %w", log.Path, err))
		}
	}
	return cleared, errors.Join(failures...)
}

func openManagedLogForWrite(path string) (*os.File, bool, error) {
	before, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("inspect managed log %q: %w", path, err)
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() {
		return nil, false, fmt.Errorf("managed log %q is not a regular file", path)
	}
	file, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return nil, false, fmt.Errorf("open managed log %q for clearing: %w", path, err)
	}
	after, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, false, fmt.Errorf("inspect opened managed log %q: %w", path, err)
	}
	if !os.SameFile(before, after) {
		_ = file.Close()
		return nil, false, fmt.Errorf("managed log %q changed while opening for clearing", path)
	}
	return file, true, nil
}

func checkpointManagedLogs(layout launchservice.Layout) managedLogCheckpoints {
	checkpoints := make(managedLogCheckpoints, 2)
	for _, log := range []managedLog{
		{Name: "stderr", Path: layout.StandardErr},
		{Name: "stdout", Path: layout.StandardOut},
	} {
		file, info, err := openManagedLog(log.Path)
		if err != nil {
			continue
		}
		_ = file.Close()
		checkpoints[log.Path] = managedLogCheckpoint{Info: info, Size: info.Size()}
	}
	return checkpoints
}

func withServiceInstallLogDiagnostics(err error, layout launchservice.Layout, checkpoints managedLogCheckpoints) error {
	if err == nil {
		return nil
	}
	var diagnostics strings.Builder
	for _, log := range []managedLog{
		{Name: "stderr", Path: layout.StandardErr},
		{Name: "stdout", Path: layout.StandardOut},
	} {
		contents, readErr := tailManagedLogSince(log.Path, checkpoints[log.Path], 50)
		if readErr != nil {
			if !errors.Is(readErr, os.ErrNotExist) {
				fmt.Fprintf(&diagnostics, "==> %s (%s) <==\nunable to read log: %v\n", log.Name, log.Path, readErr)
			}
			continue
		}
		if len(contents) == 0 {
			continue
		}
		fmt.Fprintf(&diagnostics, "==> %s (%s) <==\n%s", log.Name, log.Path, contents)
		if contents[len(contents)-1] != '\n' {
			diagnostics.WriteByte('\n')
		}
	}
	if diagnostics.Len() == 0 {
		return err
	}
	return fmt.Errorf("%w\nservice output from this install attempt:\n%s", err, strings.TrimSuffix(diagnostics.String(), "\n"))
}

func tailManagedLogSince(path string, checkpoint managedLogCheckpoint, lines int) ([]byte, error) {
	file, info, err := openManagedLog(path)
	if err != nil {
		return nil, err
	}
	defer file.Close() //nolint:errcheck // Best-effort cleanup.

	start := int64(0)
	if checkpoint.Info != nil && os.SameFile(checkpoint.Info, info) && info.Size() >= checkpoint.Size {
		start = checkpoint.Size
	}
	if start == info.Size() || lines <= 0 {
		return nil, nil
	}
	cropped := false
	if info.Size()-start > maxTailWindow {
		start = info.Size() - maxTailWindow
		cropped = true
	}
	if _, err := file.Seek(start, io.SeekStart); err != nil {
		return nil, fmt.Errorf("seek managed log %q: %w", path, err)
	}
	contents, err := io.ReadAll(io.LimitReader(file, maxTailWindow))
	if err != nil {
		return nil, fmt.Errorf("read managed log %q: %w", path, err)
	}
	if cropped {
		if newline := bytes.IndexByte(contents, '\n'); newline >= 0 {
			contents = contents[newline+1:]
		}
	}
	return lastLines(contents, lines), nil
}

func tailManagedLog(path string, lines int) ([]byte, os.FileInfo, error) {
	file, info, err := openManagedLog(path)
	if err != nil {
		return nil, nil, err
	}
	defer file.Close() //nolint:errcheck // Best-effort cleanup.
	if lines == 0 || info.Size() == 0 {
		return nil, info, nil
	}
	start := info.Size() - maxTailWindow
	if start < 0 {
		start = 0
	}
	contents, err := readManagedLogRange(file, path, start, info.Size())
	if err != nil {
		return nil, nil, err
	}
	if start > 0 {
		if newline := bytes.IndexByte(contents, '\n'); newline >= 0 {
			contents = contents[newline+1:]
		}
	}
	contents = lastLines(contents, lines)
	return contents, info, nil
}

func readManagedLogRange(file *os.File, path string, start, end int64) ([]byte, error) {
	if start < 0 || end < start {
		return nil, fmt.Errorf("invalid managed log range [%d,%d) for %q", start, end, path)
	}
	if _, err := file.Seek(start, io.SeekStart); err != nil {
		return nil, fmt.Errorf("seek managed log %q: %w", path, err)
	}
	contents, err := io.ReadAll(io.LimitReader(file, end-start))
	if err != nil {
		return nil, fmt.Errorf("read managed log %q: %w", path, err)
	}
	return contents, nil
}

func openManagedLog(path string) (*os.File, os.FileInfo, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, nil, fmt.Errorf("inspect managed log %q: %w", path, err)
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("managed log %q is not a regular file", path)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, nil, fmt.Errorf("open managed log %q: %w", path, err)
	}
	after, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, nil, fmt.Errorf("inspect opened managed log %q: %w", path, err)
	}
	if !os.SameFile(before, after) {
		_ = file.Close()
		return nil, nil, fmt.Errorf("managed log %q changed while opening", path)
	}
	return file, after, nil
}

func lastLines(contents []byte, count int) []byte {
	if count <= 0 || len(contents) == 0 {
		return nil
	}
	trimmed := bytes.TrimSuffix(contents, []byte{'\n'})
	if len(trimmed) == 0 {
		return contents
	}
	start := len(trimmed)
	for range count {
		index := bytes.LastIndexByte(trimmed[:start], '\n')
		if index < 0 {
			start = 0
			break
		}
		start = index
		if start == 0 {
			break
		}
	}
	if start > 0 && trimmed[start] == '\n' {
		start++
	}
	return contents[start:]
}

func followManagedLogs(ctx context.Context, logs []followedLog, output io.Writer) error {
	return followManagedLogsAtInterval(ctx, logs, output, 250*time.Millisecond)
}

func followManagedLogsAtInterval(ctx context.Context, logs []followedLog, output io.Writer, interval time.Duration) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.Canceled) {
				return nil
			}
			return ctx.Err()
		case <-ticker.C:
			for index := range logs {
				log := &logs[index]
				file, info, err := openManagedLog(log.Path)
				if errors.Is(err, os.ErrNotExist) {
					continue
				}
				if err != nil {
					return err
				}
				if log.Info == nil || !os.SameFile(log.Info, info) || info.Size() < log.Offset {
					log.Offset = 0
				}
				if info.Size() == log.Offset {
					log.Info = info
					_ = file.Close()
					continue
				}
				start := log.Offset
				contents, err := readManagedLogRange(file, log.Path, start, info.Size())
				_ = file.Close()
				if err != nil {
					return err
				}
				log.Offset = start + int64(len(contents))
				log.Info = info
				if len(contents) == 0 {
					continue
				}
				if len(logs) > 1 {
					if _, err := fmt.Fprintf(output, "==> %s <==\n", log.Name); err != nil {
						return fmt.Errorf("write managed log header: %w", err)
					}
				}
				if _, err := output.Write(contents); err != nil {
					return fmt.Errorf("write managed log %q: %w", log.Path, err)
				}
			}
		}
	}
}
