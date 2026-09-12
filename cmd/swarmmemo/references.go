package main

import (
	"errors"
	"os"
	"strconv"

	"swarmmemo/internal/references"
)

func referenceReaderFromEnvironment() (*references.Reader, error) {
	config := references.Config{
		RegistryPath: os.Getenv("REFERENCE_REGISTRY_PATH"), SuppressionPath: os.Getenv("REFERENCE_SUPPRESSION_PATH"),
		SnapshotPath: os.Getenv("REFERENCE_SNAPSHOT_PATH"),
	}
	rawUID := os.Getenv("REFERENCE_OWNER_UID")
	if config.RegistryPath == "" && config.SuppressionPath == "" && config.SnapshotPath == "" && rawUID == "" {
		return nil, nil
	}
	uid, err := strconv.ParseUint(rawUID, 10, 32)
	if err != nil || strconv.FormatUint(uid, 10) != rawUID {
		return nil, errors.New("REFERENCE_OWNER_UID must be an explicit canonical numeric source-writer UID")
	}
	if config.RegistryPath == "" || config.SuppressionPath == "" || config.SnapshotPath == "" {
		return nil, errors.New("all three REFERENCE file paths are required when references are configured")
	}
	config.OwnerUID = uint32(uid)
	return references.New(config)
}
