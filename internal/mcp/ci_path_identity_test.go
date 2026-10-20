package mcp

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/pathkey"
)

func TestCheckoutBatchReceiptUsesCanonicalIdentityThroughDirectoryAlias(t *testing.T) {
	root := t.TempDir()
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Skipf("directory symlink unavailable: %v", err)
	}
	file := filepath.Join(root, "source.go")
	require.NoError(t, os.WriteFile(file, []byte("package fixture\n"), 0o600))
	ctx := withCheckoutMutation(context.Background(), &fakeCheckoutMutationLifecycle{}, alias)
	scope := checkoutMutationFromContext(ctx)
	scope.checkoutID, scope.incarnation = "selected", "original"
	require.Equal(t, pathkey.CanonicalExistingRoot(root), scope.root)
	receipt := batchTransactionReceipt{
		CheckoutID: scope.checkoutID, CheckoutIncarnation: scope.incarnation, CheckoutRoot: scope.root,
		Files: []batchTransactionFile{{Path: filepath.Join(alias, "source.go")}},
	}
	require.NoError(t, guardBatchCheckout(ctx, receipt))

	restored := withCheckoutMutation(context.Background(), &fakeCheckoutMutationLifecycle{}, root)
	restoredScope := checkoutMutationFromContext(restored)
	restoredScope.checkoutID, restoredScope.incarnation = scope.checkoutID, scope.incarnation
	assert.NoError(t, guardBatchCheckout(restored, receipt))
	restoredScope.incarnation = "recreated"
	assert.Error(t, guardBatchCheckout(restored, receipt))
	restoredScope.incarnation = scope.incarnation
	receipt.CheckoutRoot = pathkey.CanonicalExistingRoot(t.TempDir())
	assert.Error(t, guardBatchCheckout(restored, receipt))
}

func TestMutationReferenceProbeUsesDirectoryAliasIdentity(t *testing.T) {
	root := t.TempDir()
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Skipf("directory symlink unavailable: %v", err)
	}
	path := filepath.Join(root, "not-created", "source.go")
	release, err := acquireMutationPath(context.Background(), path)
	require.NoError(t, err)
	t.Cleanup(release)
	assert.Equal(t, 1, mutationRefs(t, path))
	assert.Equal(t, 1, mutationRefs(t, filepath.Join(alias, "not-created", "source.go")))
}
