package main

// Owned by builder B (RFC0012 §12.1): the parameter and allowance operator
// commands. They work whatever ALLOWANCE_LEDGER is, so parameters can be
// set before the ledger is turned on.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"

	"swarmmemo/internal/allowance"
	"swarmmemo/internal/board"
)

const (
	paramsUsage    = "usage: swarmmemo params set NAMESPACE FILE --reason TEXT [--effective UNIX] | params show NAMESPACE [VERSION]"
	allowanceUsage = "usage: swarmmemo allowance grant AGENT RESOURCE UNITS --reason TEXT [--bucket granted|earned] | allowance sweep"
)

// allowanceFlags splits positional arguments from --name value pairs; every flag
// takes a value and may appear once.
func allowanceFlags(args []string, allowed ...string) (positional []string, values map[string]string, err error) {
	values = map[string]string{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if len(a) < 3 || a[:2] != "--" {
			positional = append(positional, a)
			continue
		}
		name := a[2:]
		known := false
		for _, k := range allowed {
			known = known || k == name
		}
		if !known {
			return nil, nil, fmt.Errorf("unknown flag %s", a)
		}
		if _, dup := values[name]; dup || i+1 >= len(args) {
			return nil, nil, fmt.Errorf("flag %s needs one value, once", a)
		}
		values[name] = args[i+1]
		i++
	}
	return positional, values, nil
}

// operatorParams is "swarmmemo params set NAMESPACE FILE --reason TEXT [--effective UNIX], params show NAMESPACE [VERSION]".
func operatorParams(ctx context.Context, store *board.Store, args []string, out io.Writer) error {
	pos, values, err := allowanceFlags(args, "reason", "effective")
	if err != nil {
		return err
	}
	switch {
	case len(pos) == 3 && pos[0] == "set":
		reason := values["reason"]
		if reason == "" {
			return errors.New("a public --reason is required: every parameter version says why")
		}
		var effective int64
		if v, ok := values["effective"]; ok {
			if effective, err = strconv.ParseInt(v, 10, 64); err != nil || effective < 0 {
				return errors.New("--effective is a Unix time")
			}
		}
		body, err := os.ReadFile(pos[2])
		if err != nil {
			return err
		}
		version, err := store.SetAllowanceParams(ctx, pos[1], body, reason, effective)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "%s parameters version %d stored; see /api/params/%s/%d\n", pos[1], version, pos[1], version)
		return nil
	case (len(pos) == 2 || len(pos) == 3) && pos[0] == "show" && len(values) == 0:
		version := int64(-1)
		if len(pos) == 3 {
			if version, err = strconv.ParseInt(pos[2], 10, 64); err != nil || version < 0 {
				return errors.New("VERSION is a whole number")
			}
		}
		pv, err := store.AllowanceParams(ctx, pos[1], version)
		if err != nil {
			return err
		}
		encoder := json.NewEncoder(out)
		encoder.SetIndent("", "  ")
		return encoder.Encode(pv)
	}
	return errors.New(paramsUsage)
}

// operatorAllowance is "swarmmemo allowance grant AGENT RESOURCE UNITS --reason TEXT, allowance sweep".
func operatorAllowance(ctx context.Context, store *board.Store, args []string, out io.Writer) error {
	pos, values, err := allowanceFlags(args, "reason", "bucket")
	if err != nil {
		return err
	}
	switch {
	case len(pos) == 4 && pos[0] == "grant":
		units, err := strconv.ParseInt(pos[3], 10, 64)
		if err != nil || units <= 0 {
			return errors.New("UNITS is a positive whole number")
		}
		bucket := allowance.Granted
		switch values["bucket"] {
		case "", "granted":
		case "earned":
			bucket = allowance.Earned
		default:
			return errors.New("--bucket is granted or earned; paid units come only from a funding adapter")
		}
		if values["reason"] == "" {
			return errors.New("a public --reason is required: grants are public journal entries")
		}
		if err = store.GrantAllowance(ctx, pos[1], allowance.Resource(pos[2]), bucket, units, values["reason"]); err != nil {
			return err
		}
		fmt.Fprintf(out, "granted %d %s to %s from the day's grant pool (%s)\n", units, pos[2], pos[1], bucket)
		return nil
	case len(pos) == 1 && pos[0] == "sweep" && len(values) == 0:
		n, err := store.SweepAllowance(ctx)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "swept %d rows\n", n)
		return nil
	}
	return errors.New(allowanceUsage)
}
