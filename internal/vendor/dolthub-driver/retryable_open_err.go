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
	"errors"

	"github.com/dolthub/dolt/go/store/nbs"
)

// isRetryableOpenErr reports whether an open failed because another process
// holds the database's exclusive lock — the one failure a retry can outwait.
//
// [LAW:one-source-of-truth] Modified by lit (README.lit-patch.md, Patch 7):
// lock contention is the only retry signal. The BackOff an embedder supplies
// is consulted only after a retryable failure, so it may treat every call as
// "a contender is waiting"; any other error is permanent and surfaces at once.
func isRetryableOpenErr(err error) bool {
	return errors.Is(err, nbs.ErrDatabaseLocked)
}
