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

func serviceLogsCommand(ctx context.Context, layout launchservice.Layout, args []string) error {
	flags := flag.NewFlagSet("service logs", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	flags.Usage = func() { fprintUsage(flags.Output(), []string{"service", "logs"}) }
	lines := 100
	flags.IntVar(&lines, "lines", 100, "number of trailing lines")
	flags.IntVar(&lines, "n", 100, "alias for -lines")
	follow := false
	flags.BoolVar(&follow, "follow", false, "follow appended log data")
	flags.BoolVar(&follow, "f", false, "alias for -follow")
	stream := flags.String("stream", "both", "stdout, stderr, or both")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("service logs received unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	if lines < 0 || lines > 10000 {
		return fmt.Errorf("service logs lines must be between 0 and 10000, got %d", lines)
	}
	logs, err := selectedManagedLogs(layout, strings.ToLower(*stream))
	if err != nil {
		return err
	}
	followers := make([]followedLog, 0, len(logs))
	for _, log := range logs {
		contents, info, err := tailManagedLog(log.Path, lines)
		if err != nil {
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
	if !follow {
		return nil
	}
	return followManagedLogs(ctx, followers)
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

func tailManagedLog(path string, lines int) ([]byte, os.FileInfo, error) {
	file, info, err := openManagedLog(path)
	if err != nil {
		return nil, nil, err
	}
	defer file.Close()
	if lines == 0 || info.Size() == 0 {
		return nil, info, nil
	}
	start := info.Size() - maxTailWindow
	if start < 0 {
		start = 0
	}
	if _, err := file.Seek(start, io.SeekStart); err != nil {
		return nil, nil, fmt.Errorf("seek managed log %q: %w", path, err)
	}
	contents, err := io.ReadAll(io.LimitReader(file, maxTailWindow))
	if err != nil {
		return nil, nil, fmt.Errorf("read managed log %q: %w", path, err)
	}
	if start > 0 {
		if newline := bytes.IndexByte(contents, '\n'); newline >= 0 {
			contents = contents[newline+1:]
		}
	}
	contents = lastLines(contents, lines)
	return contents, info, nil
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
		file.Close()
		return nil, nil, fmt.Errorf("inspect opened managed log %q: %w", path, err)
	}
	if !os.SameFile(before, after) {
		file.Close()
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

func followManagedLogs(ctx context.Context, logs []followedLog) error {
	ticker := time.NewTicker(250 * time.Millisecond)
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
				if err != nil {
					return err
				}
				if !os.SameFile(log.Info, info) || info.Size() < log.Offset {
					log.Offset = 0
				}
				if info.Size() == log.Offset {
					log.Info = info
					file.Close()
					continue
				}
				if _, err := file.Seek(log.Offset, io.SeekStart); err != nil {
					file.Close()
					return fmt.Errorf("seek managed log %q: %w", log.Path, err)
				}
				contents, err := io.ReadAll(file)
				file.Close()
				if err != nil {
					return fmt.Errorf("follow managed log %q: %w", log.Path, err)
				}
				if len(logs) > 1 {
					fmt.Printf("==> %s <==\n", log.Name)
				}
				_, _ = os.Stdout.Write(contents)
				log.Offset = info.Size()
				log.Info = info
			}
		}
	}
}
