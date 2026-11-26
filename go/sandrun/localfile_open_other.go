// SPDX-License-Identifier: Apache-2.0

//go:build !unix

package sandrun

import "os"

const syncFileOpenFlags = os.O_RDONLY
