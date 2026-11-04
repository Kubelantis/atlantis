package server

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/runatlantis/atlantis/server/core/config/valid"
	"github.com/runatlantis/atlantis/server/core/logstore"
	"github.com/runatlantis/atlantis/server/core/objstore"
	"github.com/runatlantis/atlantis/server/core/runtime"
	"github.com/runatlantis/atlantis/server/logging"
	tally "github.com/uber-go/tally/v4"
)

// externalPlanStore reports whether plans are kept where every replica can
// read them.
func externalPlanStore(userConfig UserConfig, globalCfg valid.GlobalCfg) bool {
	return userConfig.EnableExternalStores && globalCfg.ExternalStores.PlanStore.Type != ""
}

// validateExternalStores fails fast on --enable-external-stores without any
// configured store.
func validateExternalStores(userConfig UserConfig, globalCfg valid.GlobalCfg) error {
	stores := globalCfg.ExternalStores
	if userConfig.EnableExternalStores && stores.PlanStore.Type == "" && stores.LogStore.Type == "" {
		return errors.New("--enable-external-stores is set but no external_stores.plan_store or external_stores.log_store is configured in the server-side repo config")
	}
	return nil
}

func objstoreConfig(c valid.S3StoreConfig) objstore.Config {
	return objstore.Config{
		Bucket:               c.Bucket,
		Region:               c.Region,
		Prefix:               c.Prefix,
		Endpoint:             c.Endpoint,
		ForcePathStyle:       c.ForcePathStyle,
		Profile:              c.Profile,
		ServerSideEncryption: c.ServerSideEncryption,
		KMSKeyID:             c.KMSKeyID,
	}
}

// newPlanStore returns the S3 plan store when configured, otherwise the local
// plan store. A plan dir outside the data dir survives the loss of a checkout,
// so plans there can be recovered after a restart without an external store.
func newPlanStore(userConfig UserConfig, globalCfg valid.GlobalCfg, logger logging.SimpleLogging, scope tally.Scope) (runtime.PlanStore, error) {
	if externalPlanStore(userConfig, globalCfg) {
		cfg := globalCfg.ExternalStores.PlanStore
		if cfg.Type != "s3" {
			return nil, fmt.Errorf("unsupported plan store type %q", cfg.Type)
		}
		logger.Info("initializing S3 plan store (bucket=%s, region=%s)", cfg.S3.Bucket, cfg.S3.Region)
		store, err := runtime.NewS3PlanStore(objstoreConfig(cfg.S3), logger)
		if err != nil {
			return nil, fmt.Errorf("initializing S3 plan store: %w", err)
		}
		// Reports atlantis_plan_store_delete_errors_total.
		store.SetScope(scope.SubScope("plan_store"))
		return store, nil
	}
	local := &runtime.LocalPlanStore{}
	if userConfig.SharePlanDir != "" && filepath.Clean(userConfig.SharePlanDir) != filepath.Clean(userConfig.DataDir) {
		local.SeparatePlanDir = userConfig.SharePlanDir
	}
	return local, nil
}

// jobLogDir is where job logs are written locally.
func jobLogDir(userConfig UserConfig) string {
	if userConfig.JobLogDir != "" {
		return userConfig.JobLogDir
	}
	return filepath.Join(userConfig.DataDir, logstore.DirName)
}

// newLogStore returns the job log store: local files, archived to S3 when
// external_stores.log_store is configured. The local store is also returned
// for replica-local maintenance (the janitor).
func newLogStore(userConfig UserConfig, globalCfg valid.GlobalCfg, logger logging.SimpleLogging) (logstore.LogStore, *logstore.FileLogStore, error) {
	local, err := logstore.NewFileLogStore(jobLogDir(userConfig), logstore.DefaultFlushInterval, logger)
	if err != nil {
		return nil, nil, err
	}
	cfg := globalCfg.ExternalStores.LogStore
	if !userConfig.EnableExternalStores || cfg.Type == "" {
		return local, local, nil
	}
	if cfg.Type != "s3" {
		return nil, nil, fmt.Errorf("unsupported log store type %q", cfg.Type)
	}
	logger.Info("archiving completed job logs to S3 (bucket=%s, region=%s)", cfg.S3.Bucket, cfg.S3.Region)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s3cfg := objstoreConfig(cfg.S3)
	client, err := objstore.NewS3(ctx, s3cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("initializing S3 job log archive: %w", err)
	}
	return logstore.NewArchiveLogStore(local, objstore.NewBucket(client, s3cfg), logger), local, nil
}
