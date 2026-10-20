package mcp

import (
	"context"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/pathkey"
)

type batchRequestIntent uint8

const (
	batchRequestExecute batchRequestIntent = iota
	batchRequestPoll
	batchRequestResume
)

// Routing and execution share strict validation: malformed polls never mutate.
func classifyBatchArguments(args map[string]any) (batchRequestIntent, error) {
	for _, envelope := range []string{"options", "arguments"} {
		if _, exists := args[envelope]; exists {
			return batchRequestExecute, fmt.Errorf("%s is only valid in the edit facade; batch_edit requires top-level arguments", envelope)
		}
	}
	flags := make(map[string]bool, 2)
	for _, name := range []string{"status_only", "resume"} {
		if raw, exists := args[name]; exists {
			value, ok := raw.(bool)
			if !ok {
				return batchRequestExecute, fmt.Errorf("%s must be a boolean", name)
			}
			flags[name] = value
		}
	}
	if flags["status_only"] && flags["resume"] {
		return batchRequestExecute, fmt.Errorf("status_only and resume cannot both be true")
	}
	id := ""
	if raw, exists := args["transaction_id"]; exists {
		var ok bool
		id, ok = raw.(string)
		if !ok {
			return batchRequestExecute, fmt.Errorf("transaction_id must be a string")
		}
	}
	edits, hasEdits := args["edits"]
	hasEdits = hasEdits && edits != nil
	intent := batchRequestExecute
	if flags["status_only"] {
		intent = batchRequestPoll
	} else if flags["resume"] || !hasEdits {
		intent = batchRequestResume
	}
	if intent != batchRequestExecute {
		if strings.TrimSpace(id) == "" {
			return intent, fmt.Errorf("transaction_id is required for status or resume")
		}
		if hasEdits {
			return intent, fmt.Errorf("status_only and resume requests cannot include edits")
		}
	}
	return intent, nil
}

func validateBatchFacadeEnvelope(args map[string]any) error {
	for _, name := range []string{"arguments", "options", "source", "context", "guard", "output"} {
		if raw, exists := args[name]; exists {
			if _, ok := raw.(map[string]any); !ok {
				return fmt.Errorf("%s must be an object", name)
			}
		}
	}
	return nil
}

func (s *Server) batchRequestPoll(req *mcp.CallToolRequest) (bool, error) {
	name, ok := s.legacyToolName(req)
	if !ok || name != "batch_edit" {
		return false, nil
	}
	args := req.GetArguments()
	if isFacadeToolName(req.Params.Name) {
		if err := validateBatchFacadeEnvelope(args); err != nil {
			return false, err
		}
		spec, _ := s.viewFacadeOperation(req)
		args = normalizeFacadeArguments(spec, args)
	}
	// Legacy calls without explicit protocol flags still enter ordinary
	// mutation routing. Its read-only authority refusal takes precedence over
	// payload validation; the handler validates before any write can occur.
	_, hasStatus := args["status_only"]
	_, hasResume := args["resume"]
	if !hasStatus && !hasResume {
		return false, nil
	}
	intent, err := classifyBatchArguments(args)
	return intent == batchRequestPoll, err
}

// Receipt reads check catalog identity, without live mutation authority or I/O
// against the transaction's files. A removed catalog checkout remains refused.
func (s *Server) guardBatchPollCheckout(ctx context.Context, receipt batchTransactionReceipt) error {
	id, incarnation, root := "", "", ""
	if scope := checkoutControlFromContext(ctx); scope != nil && scope.CheckoutScoped {
		id, incarnation, root = scope.Checkout.CheckoutID, scope.Checkout.Incarnation, scope.Checkout.RootPath
	} else if scope := checkoutMutationFromContext(ctx); scope != nil {
		id, incarnation, root = scope.checkoutID, scope.incarnation, scope.root
	}
	if receipt.CheckoutID == "" {
		if id != "" {
			control := checkoutControlFromContext(ctx)
			if control == nil || control.Checkout.EffectiveMode != store_sqlite.CheckoutModeDedicated || receipt.OriginCheckoutID == "" || receipt.OriginCheckoutID != id || receipt.OriginCheckoutIncarnation == "" || receipt.OriginCheckoutIncarnation != incarnation || !pathkey.EqualPaths(receipt.OriginCheckoutRoot, root) {
				return fmt.Errorf("canonical batch requires its recorded primary checkout incarnation")
			}
			primary, err := s.batchCanonicalPrimary(ctx, control.Checkout, control.GraphID)
			if err != nil {
				return err
			}
			if !primary {
				return fmt.Errorf("canonical batch does not belong to the selected primary graph")
			}
			for _, file := range receipt.Files {
				if !pathContainedIn(file.Path, root) {
					return fmt.Errorf("batch transaction path is outside its original checkout")
				}
			}
		}
		return nil
	}
	if id == "" || id != receipt.CheckoutID || incarnation == "" || incarnation != receipt.CheckoutIncarnation || !pathkey.EqualPaths(root, receipt.CheckoutRoot) {
		return fmt.Errorf("batch transaction requires its original checkout incarnation")
	}
	for _, file := range receipt.Files {
		if !pathContainedIn(file.Path, root) {
			return fmt.Errorf("batch transaction path is outside its original checkout")
		}
	}
	return nil
}

// Poll never joins the graph owner or repairs disk/journal state. Only cached
// daemon-local completion handles can enrich the returned snapshot.
func (s *Server) pollBatchTransaction(ctx context.Context, transactionID string) (batchTransactionReceipt, error) {
	if err := ctx.Err(); err != nil {
		return batchTransactionReceipt{}, err
	}
	id := strings.TrimSpace(transactionID)
	var receipt batchTransactionReceipt
	cached := false
	if value, ok := s.batchTransactions.Load(id); ok {
		state, valid := value.(*batchTransactionState)
		if !valid {
			return receipt, fmt.Errorf("invalid transaction state for %q", id)
		}
		receipt, cached = state.snapshot(), true
	} else {
		var found bool
		var err error
		receipt, found, err = readBatchManifest(id)
		if err != nil {
			return receipt, err
		}
		if !found {
			return receipt, fmt.Errorf("transaction %q not found", id)
		}
	}
	if err := s.guardBatchPollCheckout(ctx, receipt); err != nil {
		return batchTransactionReceipt{}, err
	}
	if receipt.Status == "preparing" || receipt.Status == "prepared" {
		// A prepared manifest precedes the first rename and cannot certify disk.
		receipt.DiskStatus = "in_flight"
		receipt.RecoveryRequired, receipt.ResumeAvailable = !cached, !cached
	} else if receipt.Status == "committed" && receipt.GraphStatus != "fresh" {
		receipt.ResumeAvailable = true
		if cached {
			s.projectBatchGraphSnapshot(&receipt)
		}
		if receipt.GraphStatus == "fresh" {
			receipt.ResumeAvailable = false
		}
	}
	return receipt, nil
}

func (s *Server) projectBatchGraphSnapshot(receipt *batchTransactionReceipt) {
	if len(receipt.Files) == 0 {
		return
	}
	graphStatus := "fresh"
	unknownStatus := "pending"
	if receipt.GraphStatus == "failed" {
		unknownStatus = "failed"
	}
	seen := make(map[string]bool, len(receipt.Files))
	for _, file := range receipt.Files {
		if file.Path == "" || file.ReindexGeneration == 0 || seen[file.Path] {
			if graphStatus != "failed" {
				graphStatus = unknownStatus
			}
			continue
		}
		seen[file.Path] = true
		value, exists := s.mutationReceipts.Load(file.ReindexReceipt)
		local, valid := value.(*mutationReceipt)
		if !exists || !valid || file.ReindexReceipt == "" || local.generation != file.ReindexGeneration || local.checkoutScoped != (receipt.CheckoutID != "") || local.checkoutID != receipt.CheckoutID || local.checkoutIncarnation != receipt.CheckoutIncarnation || (receipt.CheckoutID == "" && !pathkey.EqualPaths(local.path, file.Path)) || (receipt.CheckoutID != "" && !pathkey.EqualPaths(local.path, receipt.CheckoutRoot)) {
			if graphStatus != "failed" {
				graphStatus = unknownStatus
			}
			continue
		}
		outcome, ok := s.mutationReceiptState(file.ReindexReceipt)
		if !ok {
			if graphStatus != "failed" {
				graphStatus = unknownStatus
			}
			continue
		}
		switch {
		case outcome.Err != nil || (!outcome.Pending && !outcome.Reindexed):
			graphStatus = "failed"
		case outcome.Pending:
			if graphStatus != "failed" {
				graphStatus = unknownStatus
			}
		}
		for i := range receipt.Results {
			result := &receipt.Results[i]
			if result.FilePath != file.RelativePath {
				continue
			}
			result.Reindexed, result.ReindexPending = outcome.Reindexed, outcome.Pending
			result.ReindexReceipt, result.ReindexGeneration, result.ReindexAppliedGeneration = outcome.Receipt, outcome.Generation, outcome.AppliedGeneration
			result.ReindexError = ""
			if outcome.Err != nil {
				result.ReindexError = outcome.Err.Error()
			}
		}
	}
	receipt.GraphStatus = graphStatus
}
