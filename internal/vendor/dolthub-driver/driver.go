// Copyright 2026 Dolthub, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package embedded

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"maps"

	"github.com/dolthub/dolt/go/cmd/dolt/commands/engine"
	"github.com/dolthub/dolt/go/cmd/dolt/errhand"
	"github.com/dolthub/dolt/go/libraries/doltcore/env"
	"github.com/dolthub/dolt/go/libraries/utils/config"
	"github.com/dolthub/dolt/go/libraries/utils/filesys"
)

const (
	DoltDriverName = "dolt"

	CommitNameParam      = "commitname"
	CommitEmailParam     = "commitemail"
	DatabaseParam        = "database"
	MultiStatementsParam = "multistatements"
	ClientFoundRowsParam = "clientfoundrows"

	// The following params are passed through to Dolt's local DB loading layer via
	// engine.SqlEngineConfig.DBLoadParams. They are presence-based flags (values are ignored).
	DisableSingletonCacheParam    = "disable_singleton_cache"
	FailOnJournalLockTimeoutParam = "fail_on_journal_lock_timeout"
)

var _ driver.Driver = (*doltDriver)(nil)
var _ driver.DriverContext = (*doltDriver)(nil)

func init() {
	sql.Register(DoltDriverName, &doltDriver{})
}

// doltDriver is a driver.Driver implementation which provides access to a dolt database on the local filesystem
type doltDriver struct {
}

// openSqlEngineForConnector exists to make OpenConnector retry behavior testable without
// needing to take actual filesystem locks. Production code should leave this nil.
var openSqlEngineForConnector func(ctx context.Context, cfg config.ReadWriteConfig, fs filesys.Filesys, dir, version string, seCfg *engine.SqlEngineConfig) (*engine.SqlEngine, error)

func openSqlEngine(ctx context.Context, cfg config.ReadWriteConfig, fs filesys.Filesys, dir, version string, seCfg *engine.SqlEngineConfig) (*engine.SqlEngine, error) {
	if openSqlEngineForConnector != nil {
		return openSqlEngineForConnector(ctx, cfg, fs, dir, version, seCfg)
	}
	// fs already switched to dir, so passing "." as a path.
	//
	// The engine's DBLoadParams must reach the env loads THEMSELVES, via the
	// carrier dEnv that MultiEnvForDirectory clones params from into every env
	// it creates — NewSqlEngine's own threading of seCfg.DBLoadParams into envs
	// happens only after MultiEnvForDirectory has already loaded the databases,
	// which is too late for params that shape the storage open itself
	// (singleton-cache bypass, journal-lock fail-fast). Passing nil here left
	// those opens on default semantics no matter what the connector requested.
	var carrier *env.DoltEnv
	if len(seCfg.DBLoadParams) > 0 {
		carrier = &env.DoltEnv{Version: version, DBLoadParams: maps.Clone(seCfg.DBLoadParams)}
	}
	mrEnv, err := loadMultiEnvFromDirWithParams(ctx, cfg, fs, ".", version, carrier)
	if err != nil {
		return nil, err
	}

	// Force each env's lazy database load now, and surface its failure as THE
	// open error. CollectDBs (inside NewSqlEngine) dereferences each env's
	// DoltDB without consulting DBLoadError, so a load that fails under
	// FailOnJournalLockTimeoutParam — the retryable nbs.ErrDatabaseLocked this
	// connector's whole retry path exists to catch — would otherwise panic on a
	// nil DB instead of reaching the caller's backoff.
	var loadErr error
	_ = mrEnv.Iter(func(name string, dEnv *env.DoltEnv) (stop bool, iterErr error) {
		if dEnv.DoltDB(ctx) == nil {
			loadErr = dEnv.DBLoadError
			if loadErr == nil {
				loadErr = fmt.Errorf("database %q failed to load", name)
			}
			return true, nil
		}
		return false, nil
	})
	if loadErr != nil {
		return nil, loadErr
	}

	return engine.NewSqlEngine(ctx, mrEnv, seCfg)
}

// Open opens and returns a connection to the datasource referenced by the string provided using the options provided.
// datasources must be in file url format:
//
//	file:///User/brian/driver/example/path?commitname=Billy%20Bob&commitemail=bb@gmail.com&database=dbname
//
// The path needs to point to a directory whose subdirectories are dolt databases.  If a "Create Database" command is
// run a new subdirectory will be created in this path.
func (d *doltDriver) Open(dsn string) (driver.Conn, error) {
	return nil, errors.New("dolt SQL driver does not support Open()")
}

func (d *doltDriver) OpenConnector(dsn string) (driver.Connector, error) {
	cfg, err := ParseDSN(dsn)
	if err != nil {
		return nil, err
	}
	return NewConnector(cfg)
}

// LoadMultiEnvFromDir looks at each subfolder of the given path as a Dolt repository and attempts to return a MultiRepoEnv
// with initialized environments for each of those subfolder data repositories. subfolders whose name starts with '.' are
// skipped.
func LoadMultiEnvFromDir(
	ctx context.Context,
	cfg config.ReadWriteConfig,
	fs filesys.Filesys,
	path, version string,
) (*env.MultiRepoEnv, error) {
	return loadMultiEnvFromDirWithParams(ctx, cfg, fs, path, version, nil)
}

// loadMultiEnvFromDirWithParams is LoadMultiEnvFromDir with an optional
// carrier dEnv whose DBLoadParams MultiEnvForDirectory applies to every env it
// creates before loading that env's database.
func loadMultiEnvFromDirWithParams(
	ctx context.Context,
	cfg config.ReadWriteConfig,
	fs filesys.Filesys,
	path, version string,
	carrier *env.DoltEnv,
) (*env.MultiRepoEnv, error) {

	multiDbDirFs, err := fs.WithWorkingDir(path)
	if err != nil {
		return nil, errhand.VerboseErrorFromError(err)
	}

	return env.MultiEnvForDirectory(ctx, cfg, multiDbDirFs, version, carrier)
}
