package languages

import (
	"bytes"
	"path/filepath"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

// QMLExtractor admits Qt Quick source to the file and literal-search indexes.
// Extraction is file-only: QML's object bindings and embedded JavaScript need
// their own grammar before they can provide trustworthy semantic symbols.
type QMLExtractor struct{}

func NewQMLExtractor() *QMLExtractor { return &QMLExtractor{} }

func (*QMLExtractor) Language() string     { return "qml" }
func (*QMLExtractor) Extensions() []string { return []string{".qml"} }

func (*QMLExtractor) Extract(filePath string, src []byte) (*parser.ExtractionResult, error) {
	return &parser.ExtractionResult{Nodes: []*graph.Node{{
		ID:        filePath,
		Kind:      graph.KindFile,
		Name:      filepath.Base(filePath),
		FilePath:  filePath,
		StartLine: 1,
		EndLine:   bytes.Count(src, []byte("\n")) + 1,
		Language:  "qml",
		Meta:      map[string]any{"extraction_level": "file"},
	}}}, nil
}
