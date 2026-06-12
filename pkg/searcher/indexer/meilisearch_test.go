package indexer

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
)

// A file with no extractable body text must still produce one document carrying the file
// name (with empty text) so it remains discoverable by name after a rebuild.
func TestBuildDocuments_EmptyTextYieldsFileNameDoc(t *testing.T) {
	docs := buildDocuments(7, 42, 99, "report.png", "", 500)

	assert.Len(t, docs, 1)
	d := docs[0]
	assert.Equal(t, "42_0", d.ID)
	assert.Equal(t, 42, d.FileID)
	assert.Equal(t, 7, d.OwnerID)
	assert.Equal(t, 99, d.EntityID)
	assert.Equal(t, 0, d.ChunkIdx)
	assert.Equal(t, "report.png", d.FileName)
	assert.Equal(t, "", d.Text)
}

// Whitespace-only text chunks to nothing, so it must also fall back to a single file-name doc.
func TestBuildDocuments_WhitespaceTextYieldsFileNameDoc(t *testing.T) {
	docs := buildDocuments(1, 2, 3, "blank.bin", "   \n\n  ", 500)

	assert.Len(t, docs, 1)
	assert.Equal(t, "2_0", docs[0].ID)
	assert.Equal(t, "blank.bin", docs[0].FileName)
	assert.Equal(t, "", docs[0].Text)
}

// Real body text is split into one document per chunk, each carrying the file name.
func TestBuildDocuments_TextYieldsChunkDocs(t *testing.T) {
	text := "first paragraph here\n\nsecond paragraph here"
	docs := buildDocuments(5, 10, 20, "notes.txt", text, 25)

	assert.GreaterOrEqual(t, len(docs), 2)
	for i, d := range docs {
		assert.Equal(t, fmt.Sprintf("10_%d", i), d.ID)
		assert.Equal(t, 10, d.FileID)
		assert.Equal(t, 5, d.OwnerID)
		assert.Equal(t, 20, d.EntityID)
		assert.Equal(t, i, d.ChunkIdx)
		assert.Equal(t, "notes.txt", d.FileName)
		assert.NotEmpty(t, d.Text)
	}
}
