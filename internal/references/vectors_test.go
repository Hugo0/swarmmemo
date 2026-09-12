package references

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"
)

func TestSharedPythonReferenceVectors(t *testing.T) {
	raw, err := os.ReadFile("../../curation/reference-vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var vectors struct {
		Version int `json:"version"`
		IDs     []struct {
			SourceID   string `json:"source_id"`
			ExternalID string `json:"external_id"`
			Hex        string `json:"canonical_utf8_hex"`
			ID         string `json:"id"`
		} `json:"id_vectors"`
		Encodings []struct {
			Name  string          `json:"name"`
			Value json.RawMessage `json:"value"`
			Hex   string          `json:"canonical_utf8_hex"`
			Hash  string          `json:"sha256"`
		} `json:"encoding_vectors"`
		Readers []struct {
			Name        string `json:"name"`
			Now         int64  `json:"now"`
			Registry    string `json:"registry_utf8"`
			Suppression string `json:"suppression_utf8"`
			Projection  string `json:"projection_utf8"`
			Valid       bool   `json:"valid"`
		} `json:"reader_vectors"`
	}
	if err := json.Unmarshal(raw, &vectors); err != nil {
		t.Fatal(err)
	}
	if vectors.Version != 1 || len(vectors.IDs) < 4 || len(vectors.Encodings) < 3 || len(vectors.Readers) < 30 {
		t.Fatal("incomplete shared vectors")
	}
	for _, vector := range vectors.IDs {
		t.Run("id/"+vector.SourceID+"/"+vector.ID[:8], func(t *testing.T) {
			id, err := ReferenceID(vector.SourceID, vector.ExternalID)
			if err != nil || id != vector.ID {
				t.Fatal(id, err)
			}
			raw := mustCanonical(t, []string{"swarmmemo.reference.v1", vector.SourceID, vector.ExternalID})
			if hex.EncodeToString(raw) != vector.Hex {
				t.Fatal("ID framing differs")
			}
		})
	}
	for _, vector := range vectors.Encodings {
		t.Run("encoding/"+vector.Name, func(t *testing.T) {
			value, err := parseJSON(vector.Value)
			if err != nil {
				t.Fatal(err)
			}
			raw := mustCanonical(t, value)
			if hex.EncodeToString(raw) != vector.Hex || hash(raw) != vector.Hash {
				t.Fatal("canonical encoding differs")
			}
		})
	}
	for _, vector := range vectors.Readers {
		t.Run("reader/"+vector.Name, func(t *testing.T) {
			snapshot, err := validate([]byte(vector.Registry), []byte(vector.Suppression), []byte(vector.Projection), vector.Now)
			if (err == nil) != vector.Valid {
				t.Fatalf("valid=%v want=%v err=%v", err == nil, vector.Valid, err)
			}
			if vector.Valid && snapshot.Digest != hash([]byte(vector.Projection)) {
				t.Fatal("snapshot digest differs")
			}
		})
	}
}
