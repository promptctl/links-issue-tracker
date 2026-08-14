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
	"testing"

	"github.com/cenkalti/backoff/v4"
	"github.com/dolthub/dolt/go/cmd/dolt/commands/engine"
	"github.com/dolthub/dolt/go/libraries/doltcore/dbfactory"
	"github.com/dolthub/dolt/go/libraries/utils/config"
	"github.com/dolthub/dolt/go/libraries/utils/filesys"
	gms "github.com/dolthub/go-mysql-server/sql"
	"github.com/stretchr/testify/require"
)

// TestConfigDBLoadParamMapping pins how the Config knobs translate into the
// engine's DBLoadParams: BackOff implies both params (upstream behavior), and
// each is independently addressable without a BackOff.
func TestConfigDBLoadParamMapping(t *testing.T) {
	cases := []struct {
		name              string
		mutate            func(*Config)
		wantCacheDisabled bool
		wantFailOnLock    bool
	}{
		{"neither by default", func(*Config) {}, false, false},
		{"backoff implies both", func(c *Config) {
			c.BackOff = backoff.WithMaxRetries(backoff.NewConstantBackOff(0), 1)
		}, true, true},
		{"cache disable alone", func(c *Config) { c.DisableSingletonCache = true }, true, false},
		{"fail-fast alone", func(c *Config) { c.FailOnJournalLockTimeout = true }, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var captured *engine.SqlEngineConfig
			prev := openSqlEngineForConnector
			t.Cleanup(func() { openSqlEngineForConnector = prev })
			openSqlEngineForConnector = func(ctx context.Context, cfg config.ReadWriteConfig, fs filesys.Filesys, dir, version string, seCfg *engine.SqlEngineConfig) (*engine.SqlEngine, error) {
				captured = seCfg
				return &engine.SqlEngine{}, nil
			}
			prevNewCtx := newLocalContextForConnector
			t.Cleanup(func() { newLocalContextForConnector = prevNewCtx })
			newLocalContextForConnector = func(se *engine.SqlEngine, ctx context.Context) (*gms.Context, error) {
				return &gms.Context{}, nil
			}

			cfg := Config{
				Directory:   t.TempDir(),
				CommitName:  "Test",
				CommitEmail: "test@example.com",
			}
			tc.mutate(&cfg)
			c, err := NewConnector(cfg)
			require.NoError(t, err)
			_, err = c.Connect(context.Background())
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, c.Close()) })

			require.NotNil(t, captured)
			_, cacheDisabled := captured.DBLoadParams[dbfactory.DisableSingletonCacheParam]
			_, failOnLock := captured.DBLoadParams[dbfactory.FailOnJournalLockTimeoutParam]
			require.Equal(t, tc.wantCacheDisabled, cacheDisabled, "DisableSingletonCacheParam")
			require.Equal(t, tc.wantFailOnLock, failOnLock, "FailOnJournalLockTimeoutParam")
		})
	}
}
