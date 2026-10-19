package graph

// RerankEdgeReader is an optional projection for ranking signals. It returns
// every incident edge, preserving multiplicity, endpoints, kind and confidence.
// Only similar_to edges carry Meta: their similarity value overrides confidence.
// Each distinct nonempty requested ID has an entry, nil when it has no edges.
// Other fields may be omitted. Callers needing complete edges must use Reader.
// Implementations must honor the same view, masks and structural validity as
// their ordinary batched reads. Readers without this capability use full reads.
type RerankEdgeReader interface {
	GetRerankOutEdgesByNodeIDs(ids []string) map[string][]*Edge
	GetRerankInEdgesByNodeIDs(ids []string) map[string][]*Edge
}
