package mcp

import (
	"context"
	"fmt"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/pathkey"
)

type batchCatalogOnlyKey struct{}

func withBatchCatalogOnlyLookup(ctx context.Context) context.Context {
	return context.WithValue(ctx, batchCatalogOnlyKey{}, true)
}

func batchCatalogOnlyLookup(ctx context.Context) bool {
	value, _ := ctx.Value(batchCatalogOnlyKey{}).(bool)
	return value
}

// batchCanonicalPrimary proves ownership from the catalog without materializing
// a view or acquiring a checkout mutation lease.
func (s *Server) batchCanonicalPrimary(ctx context.Context, checkout store_sqlite.Checkout, graphID string) (bool, error) {
	if s.materializer == nil || s.materializer.Catalog == nil || checkout.EffectiveMode != store_sqlite.CheckoutModeDedicated || checkout.CheckoutID == "" || checkout.Incarnation == "" || checkout.RootPath == "" {
		return false, nil
	}
	dedicated, found, err := s.materializer.Catalog.GetDedicatedGraphByOwner(ctx, checkout.CheckoutID)
	if err != nil {
		return false, err
	}
	return found && dedicated.IsPrimaryBase && dedicated.OwnerCheckoutID == checkout.CheckoutID && dedicated.FamilyID == checkout.FamilyID && dedicated.GraphID != "" && graphID == dedicated.GraphID, nil
}

// Every execution/recovery entry checks resolved explicit authority before
// loading a recovery owner or touching graph/disk state. An implicit canonical
// request keeps the existing unscoped recovery contract.
func (s *Server) guardBatchTransactionAuthority(ctx context.Context, receipt batchTransactionReceipt) error {
	if err := guardBatchCheckout(ctx, receipt); err != nil {
		return err
	}
	if receipt.CheckoutID != "" {
		return nil
	}
	view := requestViewFromContext(ctx)
	// A path-only internal context carries confinement data, not an explicit
	// selected authority. Its filesystem guards still run in the transaction.
	if view == nil || view.rider == nil {
		return nil
	}
	if !view.rider.Exact || view.rider.CheckoutID == "" || view.viewRoot == "" || s.materializer == nil || s.materializer.Catalog == nil {
		return fmt.Errorf("canonical batch requires exact recorded primary authority")
	}
	ctx = withBatchCatalogOnlyLookup(ctx)
	checkout, found, err := s.materializer.Catalog.GetCheckout(ctx, view.rider.CheckoutID)
	if err != nil {
		return err
	}
	if !found || !pathkey.EqualPaths(checkout.RootPath, view.viewRoot) {
		return fmt.Errorf("canonical batch origin checkout is unavailable or changed")
	}
	control := &checkoutControlScope{Checkout: checkout, GraphID: view.rider.GraphID, CheckoutScoped: true}
	return s.guardBatchPollCheckout(withCheckoutControl(ctx, control), receipt)
}

// Canonical batches keep their existing publication path. Origin metadata is
// separate from CheckoutID, which selects coordinator-backed worktree refresh.
// No catalog means no origin claim; legacy unscoped polling remains available.
func (s *Server) stampBatchCanonicalOrigin(ctx context.Context, receipt *batchTransactionReceipt) error {
	if s.materializer == nil || s.materializer.Catalog == nil {
		return nil
	}
	ctx = withBatchCatalogOnlyLookup(ctx)
	view := requestViewFromContext(ctx)
	var checkout store_sqlite.Checkout
	var found bool
	var err error
	graphID := ""
	if view != nil {
		if view.routed() || view.rider == nil || !view.rider.Exact || view.rider.CheckoutID == "" {
			return nil
		}
		checkout, found, err = s.materializer.Catalog.GetCheckout(ctx, view.rider.CheckoutID)
		if err != nil {
			return err
		}
		if !found || !pathkey.EqualPaths(checkout.RootPath, view.viewRoot) {
			return fmt.Errorf("canonical batch origin checkout is unavailable or changed")
		}
		graphID = view.rider.GraphID
	} else {
		// Dedicated primary CWD requests use the canonical graph and have no
		// requestView. Resolve only already registered catalog authority.
		cwd := SessionCWDFromContext(ctx)
		if cwd == "" {
			return nil
		}
		checkout, found, err = s.registeredCheckoutForPath(ctx, cwd)
		if err != nil {
			return err
		}
		if !found {
			return nil
		}
		if err := checkoutControlRootOwnsPath(checkout.RootPath, cwd); err != nil {
			return err
		}
		if err := s.checkoutInSessionScope(ctx, checkout); err != nil {
			return err
		}
		if !pathContainedIn(cwd, checkout.RootPath) {
			return fmt.Errorf("canonical batch session is outside its origin checkout")
		}
		dedicated, present, graphErr := s.materializer.Catalog.GetDedicatedGraphByOwner(ctx, checkout.CheckoutID)
		if graphErr != nil {
			return graphErr
		}
		if !present {
			return nil
		}
		graphID = dedicated.GraphID
	}
	primary, err := s.batchCanonicalPrimary(ctx, checkout, graphID)
	if err != nil {
		return err
	}
	if !primary {
		return nil
	}
	receipt.OriginCheckoutID = checkout.CheckoutID
	receipt.OriginCheckoutIncarnation = checkout.Incarnation
	receipt.OriginCheckoutRoot = checkout.RootPath
	return nil
}
