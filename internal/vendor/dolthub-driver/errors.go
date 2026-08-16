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
	"github.com/dolthub/go-mysql-server/sql"
)

// translateError converts a go-mysql-server error into this driver's
// *MySQLError. This improves compatibility with clients that program against
// embedded and sql-server Dolt.
//
// [LAW:one-source-of-truth] Modified by lit: upstream constructed
// github.com/go-sql-driver/mysql's MySQLError here, which made an MPL-2.0
// coordinate a permanent row in lit's SBOM to carry two fields across a package
// boundary between two modules lit already owns. The error contract now belongs
// to this driver — see mysql_error.go and README.lit-patch.md, Patch 4.
func translateError(err error) error {
	if err == nil {
		return nil
	}
	vitessErr := sql.CastSQLError(err)
	return &MySQLError{
		Number:  uint16(vitessErr.Num),
		Message: vitessErr.Message,
	}
}
