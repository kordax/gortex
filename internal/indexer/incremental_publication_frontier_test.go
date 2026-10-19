package indexer

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestDerivedPublicationFrontierPreservesSemanticClassification(t *testing.T) {
	prior := derivedFingerprints{declarations: "old", imports: "imports", runtime: "runtime", artifacts: "artifacts"}
	fresh := prior
	fresh.declarations = "new"
	plan := derivedPlanForDelta(prior, fresh, true, "repo/file.go", nil, nil)
	assert.Equal(t, DerivedInvalidatesDeclarations, plan.Flags)
	assert.Equal(t, []string{"repo/file.go"}, plan.ReplacedFiles, "unchanged runtime edges still need restoration after publication")

	plan.Merge(DerivedInvalidationPlan{Files: []string{"repo/metadata.go"}, MetadataOnlyFiles: 1})
	plan.Merge(DerivedInvalidationPlan{Files: []string{"repo/other.go"}, ReplacedFiles: []string{"repo/other.go"}})
	plan.Merge(DerivedInvalidationPlan{ReplacedFiles: []string{"repo/file.go"}})
	assert.Equal(t, []string{"repo/file.go", "repo/metadata.go", "repo/other.go"}, plan.Files)
	assert.Equal(t, []string{"repo/file.go", "repo/other.go"}, plan.ReplacedFiles, "metadata-only siblings must not enter the replacement frontier")
	assert.Equal(t, 1, plan.MetadataOnlyFiles)
	assert.Equal(t, DerivedInvalidatesDeclarations, plan.Flags, "restoration must not invent semantic changes")
	assert.False(t, (DerivedInvalidationPlan{ReplacedFiles: []string{"repo/file.go"}}).Empty())
}
