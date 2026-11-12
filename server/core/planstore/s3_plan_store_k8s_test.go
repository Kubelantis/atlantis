package planstore_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/runatlantis/atlantis/server/core/objstore"
	"github.com/runatlantis/atlantis/server/core/objstore/objstoretest"
	"github.com/runatlantis/atlantis/server/core/planstore"
	"github.com/runatlantis/atlantis/server/core/runtime"
	"github.com/runatlantis/atlantis/server/logging"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPlanFilenameMatchesRuntime(t *testing.T) {
	for _, c := range [][2]string{{"default", ""}, {"staging", "app"}, {"prod", "team/app"}} {
		assert.Equal(t, runtime.GetPlanFilename(c[0], c[1]), planstore.PlanFilename(c[0], c[1]))
	}
}

func TestSaveEncryptsPlans(t *testing.T) {
	fake := objstoretest.New()
	store := planstore.NewS3PlanStoreWithBucket(objstore.NewBucket(fake, objstore.Config{Bucket: "b", ServerSideEncryption: "AES256"}), logging.NewNoopLogger(t))
	planPath := filepath.Join(t.TempDir(), "default.tfplan")
	require.NoError(t, os.WriteFile(planPath, []byte("plan"), 0o600))
	require.NoError(t, store.Save(testProjectContext(), planPath))
	for _, o := range fake.Objects {
		assert.Equal(t, "AES256", string(o.Encryption))
	}
}

// A replica on the new commit must not clobber a plan it cannot use.
func TestLoadOfStalePlanKeepsExistingFile(t *testing.T) {
	fake := objstoretest.New()
	store := planstore.NewS3PlanStoreWithBucket(objstore.NewBucket(fake, objstore.Config{Bucket: "b"}), logging.NewNoopLogger(t))
	ctx := testProjectContext()
	dir := t.TempDir()
	planPath := filepath.Join(dir, "default.tfplan")
	require.NoError(t, os.WriteFile(planPath, []byte("old"), 0o600))
	require.NoError(t, store.Save(ctx, planPath))

	ctx.Pull.HeadCommit = "different"
	require.NoError(t, os.WriteFile(planPath, []byte("local"), 0o600))
	assert.ErrorContains(t, store.Load(ctx, planPath), "run plan again")
	body, _ := os.ReadFile(planPath)
	assert.Equal(t, "local", string(body))
	entries, _ := os.ReadDir(dir)
	assert.Len(t, entries, 1, "no temporary files left behind")
}

func TestRestoreListAndDeleteAcrossPages(t *testing.T) {
	fake := objstoretest.New()
	store := planstore.NewS3PlanStoreWithBucket(objstore.NewBucket(fake, objstore.Config{Bucket: "b", Prefix: "plans"}), logging.NewNoopLogger(t))
	ctx := testProjectContext()
	src := t.TempDir()
	for _, ws := range []string{"a", "b", "c"} {
		ctx.Workspace = ws
		p := filepath.Join(src, ws+".tfplan")
		require.NoError(t, os.WriteFile(p, []byte(ws), 0o600))
		require.NoError(t, store.Save(ctx, p))
	}
	ws, err := store.ListWorkspaces(ctx.BaseRepo, ctx.Pull.Num)
	require.NoError(t, err)
	assert.Equal(t, []string{"a", "b", "c"}, ws)

	dst := t.TempDir()
	require.NoError(t, store.RestorePlans(dst, ctx.BaseRepo, ctx.Pull.Num))
	var restored []string
	require.NoError(t, filepath.WalkDir(dst, func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			restored = append(restored, path)
		}
		return err
	}))
	assert.Len(t, restored, 3)

	require.NoError(t, store.DeleteForPull(ctx.BaseRepo, ctx.Pull.Num, nil))
	assert.Empty(t, fake.Keys())
}
