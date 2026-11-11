package planstore

import "github.com/runatlantis/atlantis/server/events/command"

// TestS3Key exposes the plan key layout to tests.
func (s *S3PlanStore) TestS3Key(ctx command.ProjectContext, planPath string) string {
	return s.s3Key(ctx, planPath, false)
}

// PlanFilename exposes planFilename to tests.
var PlanFilename = planFilename
