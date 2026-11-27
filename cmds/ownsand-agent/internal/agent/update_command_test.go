// SPDX-License-Identifier: FSL-1.1-ALv2

package agent

import (
	"testing"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/update"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

func TestUpdateInvalidationCommandCodes(t *testing.T) {
	for _, err := range []error{update.ErrSuperseded, update.ErrRemovalAborted} {
		detail := errorDetail(updateCommandError(err))
		if detail.GetCode() != ownsandv1.ErrorCode_ERROR_CODE_ABORTED || detail.GetMessage() != err.Error() {
			t.Errorf("%v mapped to %v, want ABORTED with the original message", err, detail)
		}
	}
}
