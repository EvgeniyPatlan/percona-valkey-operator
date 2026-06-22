/*
Copyright Percona LLC.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Command valkey-backup is the backup/restore SIDECAR that runs in a Kubernetes
// Job pod inside the DB image (percona/percona-valkey), not the operator image.
// It has three modes:
//
//   - backup  (default): resolve each shard's live primary from CLUSTER NODES,
//     pull each shard's RDB over the replication protocol (SYNC, acting as a
//     replica — see rdbsource.go / open question CR-10), stream it to object
//     storage with an incremental SHA-256, and write the backup-set manifest
//     LAST (06 §4).
//   - --download --shard=<i>: the restore-seed init container — fetch one shard's
//     RDB, verify its SHA-256 against the manifest, write /data/dump.rdb (06 §7.4).
//   - --cleanup: the finalizer teardown — delete the set manifest FIRST, then the
//     RDBs (06 §6.1).
//
// All Valkey and object-store interaction happens HERE in the Job, never in the
// operator process (narrow RBAC, isolated data movement, 06 §4.1). Credentials
// arrive as mounted-Secret env vars and are consumed only here.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	valkeyv1alpha1 "valkey.percona.com/percona-valkey-operator/pkg/apis/valkey/v1alpha1"
	"valkey.percona.com/percona-valkey-operator/pkg/backup"
	"valkey.percona.com/percona-valkey-operator/pkg/naming"
	"valkey.percona.com/percona-valkey-operator/pkg/valkey"
)

// Environment variable names the Job's container reads (set by the backup/restore
// controller's Job builder). Storage credentials are read by pkg/backup backends
// from their own env (AWS_*/AZURE_*/GOOGLE_*); these carry the operation inputs.
//
// The canonical names live in pkg/backup (the single source of truth shared with
// the controller Job builders, so the two sides cannot drift); these locals alias
// them so the sidecar code and its tests read tersely.
const (
	envCluster     = backup.EnvCluster
	envBackupName  = backup.EnvBackupName
	envMode        = backup.EnvMode
	envCRVersion   = backup.EnvCRVersion
	envConsistency = backup.EnvConsistency
	envStorageType = backup.EnvStorageType
	envS3Bucket    = backup.EnvS3Bucket
	envS3Prefix    = backup.EnvS3Prefix
	envS3Region    = backup.EnvS3Region
	envS3Endpoint  = backup.EnvS3Endpoint
	envGCSBucket   = backup.EnvGCSBucket
	envGCSPrefix   = backup.EnvGCSPrefix
	envAzureCtr    = backup.EnvAzureContainer
	envAzurePrefix = backup.EnvAzurePrefix
	envFSRoot      = backup.EnvFSRoot
	envSeedNode    = backup.EnvSeedNode
	envAuthUser    = backup.EnvAuthUser
	// envAuthPassword is the env-var NAME the Job reads the _backup password
	// from (M6 security refactor, 07 §10); the value is a mounted-Secret
	// reference, never embedded in source.
	envAuthPassword  = backup.EnvAuthPassword
	envTLSEnabled    = backup.EnvTLSEnabled
	envTLSCAFile     = backup.EnvTLSCAFile
	envDownloadDst   = backup.EnvDownloadDst
	envEngineVersion = backup.EnvEngineVersion
)

// cliFlags are the per-mode flags; env vars carry the bulk of the configuration
// so the controller's Job builder sets a small, stable flag surface.
type cliFlags struct {
	download bool
	cleanup  bool
	shard    int
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "valkey-backup: %v\n", err)
		os.Exit(1)
	}
}

// run parses flags, builds the resolved options from the environment, and
// dispatches to the selected mode. It is separated from main for testability.
func run(args []string) error {
	flags, err := parseFlags(args)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	switch {
	case flags.cleanup:
		return runCleanupFromEnv(ctx)
	case flags.download:
		return runDownloadFromEnv(ctx, flags.shard)
	default:
		return runBackupFromEnv(ctx)
	}
}

// parseFlags parses the small mode-selecting flag surface.
func parseFlags(args []string) (cliFlags, error) {
	fs := flag.NewFlagSet("valkey-backup", flag.ContinueOnError)
	var f cliFlags
	fs.BoolVar(&f.download, "download", false, "restore-seed mode: download one shard's RDB to /data/dump.rdb")
	fs.BoolVar(&f.cleanup, "cleanup", false, "finalizer teardown mode: delete the backup-set (manifest first)")
	fs.IntVar(&f.shard, "shard", -1, "shard index to download (with --download)")
	if err := fs.Parse(args); err != nil {
		return cliFlags{}, err
	}
	if f.download && f.shard < 0 {
		return cliFlags{}, fmt.Errorf("--download requires --shard=<i>")
	}
	return f, nil
}

// buildStore resolves the StorageConfig from the environment (storage type +
// coordinates; credential VALUES are read by the backend from AWS_*/AZURE_*/
// GOOGLE_* env directly) and constructs the ArtifactStore via the seam.
func buildStore(ctx context.Context) (backup.ArtifactStore, error) {
	cfg, err := storageConfigFromEnv()
	if err != nil {
		return nil, err
	}
	return backup.NewStore(ctx, cfg)
}

// storageConfigFromEnv builds the StorageConfig from VALKEY_BACKUP_* env. The
// Credentials map is populated from the cloud-SDK env names so the backends find
// the mounted-Secret values (06 §8.2).
func storageConfigFromEnv() (backup.StorageConfig, error) {
	switch valkeyv1alpha1.BackupStorageType(strings.ToLower(os.Getenv(envStorageType))) {
	case valkeyv1alpha1.BackupStorageS3:
		return backup.StorageConfig{
			Type:        valkeyv1alpha1.BackupStorageS3,
			S3:          s3SpecFromEnv(),
			Credentials: credsFromEnv(),
		}, nil
	case valkeyv1alpha1.BackupStorageGCS:
		return backup.StorageConfig{
			Type:        valkeyv1alpha1.BackupStorageGCS,
			GCS:         gcsSpecFromEnv(),
			Credentials: credsFromEnv(),
		}, nil
	case valkeyv1alpha1.BackupStorageAzure:
		return backup.StorageConfig{
			Type:        valkeyv1alpha1.BackupStorageAzure,
			Azure:       azureSpecFromEnv(),
			Credentials: credsFromEnv(),
		}, nil
	case valkeyv1alpha1.BackupStorageFilesystem:
		return backup.StorageConfig{
			Type:           valkeyv1alpha1.BackupStorageFilesystem,
			FilesystemRoot: os.Getenv(envFSRoot),
		}, nil
	default:
		return backup.StorageConfig{}, fmt.Errorf("unknown or empty %s=%q", envStorageType, os.Getenv(envStorageType))
	}
}

// runBackupFromEnv resolves the cluster topology + store from the environment
// and runs the backup loop.
func runBackupFromEnv(ctx context.Context) error {
	store, err := buildStore(ctx)
	if err != nil {
		return fmt.Errorf("init store: %w", err)
	}
	auth, tlsConfig, err := connSecurityFromEnv()
	if err != nil {
		return err
	}
	shards, err := resolveShards(ctx, os.Getenv(envSeedNode), auth, tlsConfig)
	if err != nil {
		return fmt.Errorf("resolve shards: %w", err)
	}
	o := backupOptions{
		cluster:       os.Getenv(envCluster),
		backupName:    os.Getenv(envBackupName),
		mode:          envOrDefault(envMode, "cluster"),
		crVersion:     os.Getenv(envCRVersion),
		engineVersion: os.Getenv(envEngineVersion),
		consistency:   envOrDefault(envConsistency, consistencyStrict),
		store:         store,
		shards:        shards,
		newRDBSource: func(sp shardPrimary) RDBSource {
			return newSyncRDBSource(sp.addr, auth, tlsConfig)
		},
	}
	return runBackup(ctx, o)
}

// runDownloadFromEnv runs the restore-seed download for one shard.
func runDownloadFromEnv(ctx context.Context, shard int) error {
	store, err := buildStore(ctx)
	if err != nil {
		return fmt.Errorf("init store: %w", err)
	}
	dstPath := envOrDefault(envDownloadDst, "/data/dump.rdb")
	f, err := os.Create(dstPath)
	if err != nil {
		return fmt.Errorf("create %s: %w", dstPath, err)
	}
	defer func() { _ = f.Close() }()

	if err = runDownload(ctx, downloadOptions{
		cluster:    os.Getenv(envCluster),
		backupName: os.Getenv(envBackupName),
		shardIndex: shard,
		store:      store,
		dst:        f,
	}); err != nil {
		// Remove a partially written seed so the engine never boots a truncated RDB.
		_ = os.Remove(dstPath)
		return err
	}
	return f.Sync()
}

// runCleanupFromEnv runs the finalizer teardown.
func runCleanupFromEnv(ctx context.Context) error {
	store, err := buildStore(ctx)
	if err != nil {
		return fmt.Errorf("init store: %w", err)
	}
	return runCleanup(ctx, cleanupOptions{
		cluster:    os.Getenv(envCluster),
		backupName: os.Getenv(envBackupName),
		store:      store,
	})
}

// connSecurityFromEnv resolves the AUTH credentials and optional TLS config the
// Job uses to talk to Valkey. The _backup credentials and CA come from the mounted
// cluster Secrets via env/file (06 §8.3). The default user is _backup (M6 security
// refactor, 07 §10): _backup carries the SYNC-as-replica replication grants that
// were moved off _operator; the operator injects VALKEY_BACKUP_AUTH_USER=_backup.
func connSecurityFromEnv() (authCreds, *tls.Config, error) {
	auth := authCreds{
		username: envOrDefault(envAuthUser, naming.SystemUserBackup),
		password: os.Getenv(envAuthPassword),
	}
	if strings.ToLower(os.Getenv(envTLSEnabled)) != "true" {
		return auth, nil, nil
	}
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	caFile := os.Getenv(envTLSCAFile)
	if caFile != "" {
		caData, err := os.ReadFile(caFile)
		if err != nil {
			return auth, nil, fmt.Errorf("read TLS CA %s: %w", caFile, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caData) {
			return auth, nil, fmt.Errorf("parse TLS CA %s", caFile)
		}
		tlsConfig.RootCAs = pool
	}
	return auth, tlsConfig, nil
}

// resolveShards scrapes CLUSTER NODES from the seed node and builds the live
// primary-per-shard list, ordered ascending by shard index (06 §4.3 step 1,
// §4.4). It connects to the seed via the high-level valkey-go client (read-only
// scrape), then derives the per-shard primary addresses for the SYNC source.
func resolveShards(ctx context.Context, seedAddr string, auth authCreds, tlsConfig *tls.Config) ([]shardPrimary, error) {
	if seedAddr == "" {
		return nil, fmt.Errorf("empty %s", envSeedNode)
	}
	client, err := valkey.NewClient(seedAddr, valkey.Auth{Username: auth.username, Password: auth.password}, tlsConfig)
	if err != nil {
		return nil, fmt.Errorf("dial seed %s: %w", seedAddr, err)
	}
	defer func() { _ = client.Close() }()

	scrapeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	raw, err := client.ClusterNodes(scrapeCtx)
	if err != nil {
		return nil, fmt.Errorf("CLUSTER NODES: %w", err)
	}
	return shardPrimariesFromClusterNodes(raw)
}

// envOrDefault returns the env value for key, or def when unset/empty.
func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
