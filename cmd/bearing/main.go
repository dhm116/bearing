// Command bearing is the Bearing developer CLI. For now it helps adapter
// authors run and check adapters:
//
//	bearing adapter describe -- <adapter command> [args]
//	bearing adapter sync --config cfg.json -- <adapter command> [args]
//	bearing validate [file.ndjson]   (reads stdin when no file is given)
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"

	"bearing.example/pkg/adapter"
	"bearing.example/pkg/model"
)

const usage = `usage:
  bearing adapter describe -- <adapter command> [args]
  bearing adapter sync [--config cfg.json] [--max-pages N] -- <adapter command> [args]
  bearing validate [file.ndjson]
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "bearing:", err)
		os.Exit(1)
	}
}

var errUsage = errors.New("see usage above")

func run(ctx context.Context, args []string, stdin io.Reader, stdout io.Writer) error {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return errUsage
	}
	switch args[0] {
	case "adapter":
		if len(args) < 2 {
			fmt.Fprint(os.Stderr, usage)
			return errUsage
		}
		switch args[1] {
		case "describe":
			return describe(ctx, args[2:], stdout)
		case "sync":
			return syncCmd(ctx, args[2:], stdout)
		}
	case "validate":
		in := stdin
		if len(args) > 1 {
			f, err := os.Open(args[1])
			if err != nil {
				return err
			}
			defer f.Close()
			in = f
		}
		n, err := validate(in)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "%d observations valid\n", n)
		return nil
	}
	fmt.Fprint(os.Stderr, usage)
	return errUsage
}

func splitCommand(args []string) ([]string, []string, error) {
	for i, a := range args {
		if a == "--" {
			if i == len(args)-1 {
				return nil, nil, errors.New("missing adapter command after --")
			}
			return args[:i], args[i+1:], nil
		}
	}
	return nil, nil, errors.New("put the adapter command after --")
}

func describe(ctx context.Context, args []string, stdout io.Writer) error {
	_, cmd, err := splitCommand(args)
	if err != nil {
		return err
	}
	c, err := adapter.Start(ctx, cmd[0], cmd[1:]...)
	if err != nil {
		return err
	}
	defer c.Close()
	d, err := c.Describe(ctx)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(d)
}

func syncCmd(ctx context.Context, args []string, stdout io.Writer) error {
	flagArgs, cmd, err := splitCommand(args)
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("sync", flag.ContinueOnError)
	configPath := fs.String("config", "", "adapter config JSON file")
	maxPages := fs.Int("max-pages", 10000, "stop if the adapter has not finished after this many pages")
	if err := fs.Parse(flagArgs); err != nil {
		return err
	}
	config := json.RawMessage(`{}`)
	if *configPath != "" {
		b, err := os.ReadFile(*configPath)
		if err != nil {
			return err
		}
		config = b
	}
	c, err := adapter.Start(ctx, cmd[0], cmd[1:]...)
	if err != nil {
		return err
	}
	defer c.Close()
	w := bufio.NewWriter(stdout)
	defer w.Flush()
	enc := json.NewEncoder(w)
	n := 0
	err = adapter.SyncAll(ctx, c, config, *maxPages, func(o model.Observation) error {
		n++
		return enc.Encode(o)
	})
	fmt.Fprintf(os.Stderr, "bearing: %d observations\n", n)
	return err
}

// validate checks newline-delimited observations and returns how many passed.
func validate(r io.Reader) (int, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 32<<20)
	n, line := 0, 0
	var errs []error
	for sc.Scan() {
		line++
		if len(sc.Bytes()) == 0 {
			continue
		}
		if _, err := model.DecodeObservation(sc.Bytes()); err != nil {
			errs = append(errs, fmt.Errorf("line %d: %w", line, err))
			continue
		}
		n++
	}
	if err := sc.Err(); err != nil {
		return n, err
	}
	return n, errors.Join(errs...)
}
