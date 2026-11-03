package board

import (
	"fmt"
	"reflect"
	"testing"
)

// ReadLogPlace is ReadLogProof without the audit paths: the same leaf,
// checkpoint, anchor, related leaves and attached text, with every Proof
// nil (C726). The JSON proof keeps its path.
func TestReadLogPlaceIsTheProofWithoutItsPath(t *testing.T) {
	s := openTest(t, Config{})
	steppingClock(s)
	_, postID, hiddenID := buildHistory(t, s)
	for i := range 20 {
		run(t, s, Command{Operation: "post", Room: "lobby", Text: fmt.Sprintf("filler %d", i)})
	}
	if _, err := s.SignCheckpoint(testContext); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		index   int64
		message string
	}{{-1, postID}, {-1, hiddenID}, {0, ""}, {3, ""}} {
		proof, err := s.ReadLogProof(testContext, c.index, c.message, -1)
		if err != nil || len(proof.Proof) == 0 {
			t.Fatalf("%v: proof %+v %v", c, proof, err)
		}
		place, err := s.ReadLogPlace(testContext, c.index, c.message, -1)
		if err != nil || place.Proof != nil {
			t.Fatalf("%v: place carries a path: %+v %v", c, place.Proof, err)
		}
		proof.Proof = nil
		for i := range proof.Related {
			if len(proof.Related[i].Proof) == 0 || place.Related[i].Proof != nil {
				t.Fatalf("%v: related %d paths: %v %v", c, i, proof.Related[i].Proof, place.Related[i].Proof)
			}
			proof.Related[i].Proof = nil
		}
		if !reflect.DeepEqual(proof, place) {
			t.Fatalf("%v: place differs from the proof:\n%+v\n%+v", c, place, proof)
		}
	}
	// Errors are the proof's too: a message the log does not hold.
	if _, err := s.ReadLogPlace(testContext, -1, "ab", -1); err == nil {
		t.Fatal("place of an unknown message")
	}
}

// BenchmarkReadLogPlace compares the proof page's read with and without the
// audit path on a log of some thousands of leaves (C726).
func BenchmarkReadLogPlace(b *testing.B) {
	s := openTest(b, Config{})
	var last string
	for i := range 3000 {
		res, err := s.Execute(testContext, Command{Operation: "post", Room: "lobby", Text: fmt.Sprintf("post %d", i)}, "test-origin")
		if err != nil {
			b.Fatal(err)
		}
		last = res.Receipt.ID
	}
	if _, err := s.SignCheckpoint(testContext); err != nil {
		b.Fatal(err)
	}
	b.Run("proof", func(b *testing.B) {
		for b.Loop() {
			if _, err := s.ReadLogProof(testContext, -1, last, -1); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("place", func(b *testing.B) {
		for b.Loop() {
			if _, err := s.ReadLogPlace(testContext, -1, last, -1); err != nil {
				b.Fatal(err)
			}
		}
	})
}
