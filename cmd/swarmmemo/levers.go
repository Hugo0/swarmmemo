package main

// Owned by builder E (RFC0012 §12.1): the steward's emergency levers (§2.6).

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"

	"swarmmemo/internal/board"
)

const leverUsage = `usage: swarmmemo lever list
       swarmmemo lever pull NAME [ARGS] --reason TEXT [--until UNIX] [--actor NAME]
       swarmmemo lever release NAME [RESOURCE|PREFIX_ID|CIDR] [--reason TEXT] [--actor NAME]
       swarmmemo lever expire
levers: tier4-shrink PPM | signed-only | pause-new-keys | cut-budget RESOURCE PPM |
        block-prefix CIDR | proven-only | freeze-transfers | signed-services
Every change is logged publicly at /api/levers; the reason is public, a blocked prefix is not.`

// operatorLever is "swarmmemo lever pull NAME [ARGS] --reason TEXT [--until UNIX], lever release NAME, lever list".
func operatorLever(ctx context.Context, store *board.Store, args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New(leverUsage)
	}
	command := args[0]
	positional, flags, err := leverArgs(args[1:])
	if err != nil {
		return err
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	switch command {
	case "list":
		if len(positional) > 0 || len(flags) > 0 {
			return errors.New(leverUsage)
		}
		if _, err := store.ExpireLevers(ctx); err != nil {
			return err
		}
		report, err := store.LeverReport(ctx)
		if err != nil {
			return err
		}
		return enc.Encode(report)
	case "expire":
		if len(positional) > 0 || len(flags) > 0 {
			return errors.New(leverUsage)
		}
		n, err := store.ExpireLevers(ctx)
		if err != nil {
			return err
		}
		return enc.Encode(map[string]int{"expired": n})
	case "pull":
		if len(positional) == 0 {
			return errors.New(leverUsage)
		}
		var until int64
		if v, ok := flags["until"]; ok {
			if until, err = strconv.ParseInt(v, 10, 64); err != nil || until <= 0 {
				return errors.New("--until takes a UNIX time in seconds")
			}
		}
		change, err := store.PullLever(ctx, board.LeverPull{Name: positional[0], Args: positional[1:], Reason: flags["reason"], Actor: flags["actor"], Until: until})
		if err != nil {
			return err
		}
		return enc.Encode(change)
	case "release":
		if len(positional) == 0 {
			return errors.New(leverUsage)
		}
		if _, ok := flags["until"]; ok {
			return errors.New("lever release takes no --until")
		}
		change, err := store.ReleaseLever(ctx, positional[0], positional[1:], flags["actor"], flags["reason"])
		if err != nil {
			return err
		}
		return enc.Encode(change)
	}
	return errors.New(leverUsage)
}

// leverArgs splits arguments into positionals and the flags --reason,
// --until and --actor, each given once, as "--flag value" or "--flag=value".
func leverArgs(args []string) ([]string, map[string]string, error) {
	var positional []string
	flags := map[string]string{}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if len(arg) < 2 || arg[:2] != "--" {
			positional = append(positional, arg)
			continue
		}
		name, value, inline := strings.Cut(arg[2:], "=")
		if name != "reason" && name != "until" && name != "actor" {
			return nil, nil, errors.New("unknown flag --" + name + "\n" + leverUsage)
		}
		if _, dup := flags[name]; dup {
			return nil, nil, errors.New("--" + name + " given twice")
		}
		if !inline {
			if i+1 >= len(args) {
				return nil, nil, errors.New("--" + name + " needs a value")
			}
			i++
			value = args[i]
		}
		flags[name] = value
	}
	return positional, flags, nil
}
