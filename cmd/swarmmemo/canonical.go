package main

import (
	"errors"
	"io"

	"swarmmemo/internal/board"
	"swarmmemo/internal/httpapi"
)

func canonicalInput(input io.Reader, service string) ([]byte, error) {
	const limit = 2 << 20
	raw, err := io.ReadAll(io.LimitReader(input, limit+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > limit {
		return nil, errors.New("canonical command exceeds 2 MiB")
	}
	command, err := httpapi.DecodeCommand(raw)
	if err != nil {
		return nil, err
	}
	return board.Canonical(service, command), nil
}
