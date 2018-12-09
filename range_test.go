package merkletree

import (
	"bytes"
	"encoding/hex"
	"hash"
	"io"
	"reflect"
	"testing"

	"github.com/HyperspaceApp/fastrand"
	"golang.org/x/crypto/blake2b"
)

// bytesRoot is a helper function that calculates the Merkle root of b.
func bytesRoot(b []byte, h hash.Hash, leafSize int) []byte {
	root, err := ReaderRoot(bytes.NewReader(b), h, leafSize)
	if err != nil {
		// should be unreachable, since ReaderRoot only reports unexpected
		// errors returned by the supplied io.Reader, and bytes.Reader does
		// not return any such errors.
		panic(err)
	}
	return root
}

// A precalcSubtreeHasher wraps an underlying SubtreeHasher. It uses
// precalculated subtree roots where possible, only falling back to the
// underlying SubtreeHasher if needed.
type precalcSubtreeHasher struct {
	precalc     [][]byte
	subtreeSize int
	h           hash.Hash
	sh          SubtreeHasher
}

func (p *precalcSubtreeHasher) NextSubtreeRoot(n int) ([]byte, error) {
	if n%p.subtreeSize == 0 && len(p.precalc) >= n/p.subtreeSize {
		np := n / p.subtreeSize
		tree := New(p.h)
		for _, root := range p.precalc[:np] {
			tree.PushSubTree(0, root)
		}
		p.precalc = p.precalc[np:]
		return tree.Root(), p.sh.Skip(n)
	}
	return p.sh.NextSubtreeRoot(n)
}

func (p *precalcSubtreeHasher) Skip(n int) error {
	skippedHashes := n / p.subtreeSize
	if n%p.subtreeSize != 0 {
		skippedHashes++
	}
	p.precalc = p.precalc[skippedHashes:]
	return p.sh.Skip(n)
}

func newPrecalcSubtreeHasher(precalc [][]byte, subtreeSize int, h hash.Hash, sh SubtreeHasher) *precalcSubtreeHasher {
	return &precalcSubtreeHasher{
		precalc:     precalc,
		subtreeSize: subtreeSize,
		h:           h,
		sh:          sh,
	}
}

// testBuildVerifyRangeProof tests the BuildRangeProof and VerifyRangeProof
// functions.
func TestBuildVerifyRangeProof(t *testing.T) {
	// setup proof parameters
	blake, _ := blake2b.New256(nil)
	leafData := make([]byte, 1<<22)
	const leafSize = 64
	numLeaves := len(leafData) / 64
	leafHashes := make([][]byte, numLeaves)
	for i := range leafHashes {
		leafHashes[i] = leafSum(blake, leafData[i*leafSize:][:leafSize])
	}
	root := bytesRoot(leafData, blake, leafSize)

	// convenience functions
	leafHash := func(leaf []byte) []byte {
		return leafSum(blake, leaf)
	}
	nodeHash := func(left, right []byte) []byte {
		return nodeSum(blake, left, right)
	}
	buildProof := func(start, end int) [][]byte {
		// flip a coin to decide whether to use leaf data or leaf hashes
		var sh SubtreeHasher
		if fastrand.Intn(2) == 0 {
			sh = NewReaderSubtreeHasher(bytes.NewReader(leafData), leafSize, blake)
		} else {
			sh = NewCachedSubtreeHasher(leafHashes, blake)
		}
		proof, err := BuildRangeProof(start, end, sh)
		if err != nil {
			t.Fatal(err)
		}
		return proof

	}
	verifyProof := func(start, end int, proof [][]byte) bool {
		// flip a coin to decide whether to use leaf data or leaf hashes
		var lh LeafHasher
		if fastrand.Intn(2) == 0 {
			lh = NewReaderLeafHasher(bytes.NewReader(leafData[start*leafSize:end*leafSize]), blake, leafSize)
		} else {
			lh = NewCachedLeafHasher(leafHashes[start:end])
		}
		ok, err := VerifyRangeProof(lh, blake, start, end, proof, root)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}

	// test some known proofs
	proof := buildProof(0, numLeaves)
	if len(proof) != 0 {
		t.Error("BuildRangeProof constructed an incorrect proof for the entire sector")
	}

	proof = buildProof(0, 1)
	checkRoot := leafHash(leafData[:leafSize])
	for i := range proof {
		checkRoot = nodeHash(checkRoot, proof[i])
	}
	if hex.EncodeToString(checkRoot) != "50ed59cecd5ed3ca9e65cec0797202091dbba45272dafa3faa4e27064eedd52c" {
		t.Error("BuildRangeProof constructed an incorrect proof for the first leaf")
	} else if !verifyProof(0, 1, proof) {
		t.Error("VerifyRangeProof failed to verify a known correct proof")
	}

	proof = buildProof(numLeaves-1, numLeaves)
	checkRoot = leafHash(leafData[len(leafData)-leafSize:])
	for i := range proof {
		checkRoot = nodeHash(proof[len(proof)-i-1], checkRoot)
	}
	if hex.EncodeToString(checkRoot) != "50ed59cecd5ed3ca9e65cec0797202091dbba45272dafa3faa4e27064eedd52c" {
		t.Error("BuildRangeProof constructed an incorrect proof for the last leaf")
	} else if !verifyProof(numLeaves-1, numLeaves, proof) {
		t.Error("VerifyRangeProof failed to verify a known correct proof")
	}

	proof = buildProof(10, 11)
	checkRoot = leafHash(leafData[10*leafSize:][:leafSize])
	checkRoot = nodeHash(checkRoot, proof[2])
	checkRoot = nodeHash(proof[1], checkRoot)
	checkRoot = nodeHash(checkRoot, proof[3])
	checkRoot = nodeHash(proof[0], checkRoot)
	for i := 4; i < len(proof); i++ {
		checkRoot = nodeHash(checkRoot, proof[i])
	}
	if hex.EncodeToString(checkRoot) != "50ed59cecd5ed3ca9e65cec0797202091dbba45272dafa3faa4e27064eedd52c" {
		t.Error("BuildRangeProof constructed an incorrect proof for a middle leaf")
	} else if !verifyProof(10, 11, proof) {
		t.Error("VerifyRangeProof failed to verify a known correct proof")
	}

	// this is the largest possible proof
	midl, midr := numLeaves/2-1, numLeaves/2+1
	proof = buildProof(midl, midr)
	left := leafHash(leafData[midl*leafSize:][:leafSize])
	for i := 0; i < len(proof)/2; i++ {
		left = nodeHash(proof[len(proof)/2-i-1], left)
	}
	right := leafHash(leafData[(midr-1)*leafSize:][:leafSize])
	for i := len(proof) / 2; i < len(proof); i++ {
		right = nodeHash(right, proof[i])
	}
	checkRoot = nodeHash(left, right)
	if hex.EncodeToString(checkRoot) != "50ed59cecd5ed3ca9e65cec0797202091dbba45272dafa3faa4e27064eedd52c" {
		t.Error("BuildRangeProof constructed an incorrect proof for worst-case inputs")
	} else if !verifyProof(midl, midr, proof) {
		t.Error("VerifyRangeProof failed to verify a known correct proof")
	}

	// for more intensive testing, use smaller trees
	buildSmallProof := func(start, end, nLeaves int) [][]byte {
		// flip a coin to decide whether to use leaf data or leaf hashes
		var sh SubtreeHasher
		if fastrand.Intn(2) == 0 {
			sh = NewReaderSubtreeHasher(bytes.NewReader(leafData[:leafSize*nLeaves]), leafSize, blake)
		} else {
			sh = NewCachedSubtreeHasher(leafHashes[:nLeaves], blake)
		}
		proof, err := BuildRangeProof(start, end, sh)
		if err != nil {
			t.Fatal(err)
		}
		return proof

	}
	verifySmallProof := func(start, end int, proof [][]byte, nLeaves int) bool {
		// flip a coin to decide whether to use leaf data or leaf hashes
		var lh LeafHasher
		if fastrand.Intn(2) == 0 {
			lh = NewReaderLeafHasher(bytes.NewReader(leafData[start*leafSize:end*leafSize]), blake, leafSize)
		} else {
			lh = NewCachedLeafHasher(leafHashes[start:end])
		}
		smallRoot := bytesRoot(leafData[:leafSize*nLeaves], blake, leafSize)
		ok, err := VerifyRangeProof(lh, blake, start, end, proof, smallRoot)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}

	// test some random proofs against VerifyRangeProof
	for nLeaves := 1; nLeaves <= 65; nLeaves++ {
		for n := 0; n < 5; n++ {
			start := fastrand.Intn(nLeaves)
			end := start + fastrand.Intn(nLeaves-start) + 1
			proof := buildSmallProof(start, end, nLeaves)
			if !verifySmallProof(start, end, proof, nLeaves) {
				t.Errorf("BuildRangeProof constructed an incorrect proof for nLeaves=%v, range %v-%v", nLeaves, start, end)
			}

			// corrupt the proof; it should fail to verify
			if len(proof) == 0 {
				continue
			}
			switch fastrand.Intn(3) {
			case 0:
				// modify an element of the proof
				proof[fastrand.Intn(len(proof))][fastrand.Intn(blake.Size())] += 1
			case 1:
				// add an element to the proof
				proof = append(proof, make([]byte, blake.Size()))
				i := fastrand.Intn(len(proof))
				proof[i], proof[len(proof)-1] = proof[len(proof)-1], proof[i]
			case 2:
				// delete a random element of the proof
				i := fastrand.Intn(len(proof))
				proof = append(proof[:i], proof[i+1:]...)
			}
			if verifyProof(start, end, proof) {
				t.Errorf("VerifyRangeProof verified an incorrect proof for nLeaves=%v, range %v-%v", nLeaves, start, end)
			}
		}
	}

	// build and verify every possible proof for a small tree
	for start := 0; start < 12; start++ {
		for end := start + 1; end <= 12; end++ {
			proof := buildSmallProof(start, end, 12)
			if !verifySmallProof(start, end, proof, 12) {
				t.Errorf("BuildRangeProof constructed an incorrect proof for range %v-%v", start, end)
			}
		}
	}

	// manually verify every hash in a proof
	//
	// NOTE: this is the same proof described in the BuildRangeProof comment:
	//
	//               ┌────────┴────────*
	//         ┌─────┴─────┐           │
	//      *──┴──┐     ┌──┴──*     ┌──┴──┐
	//    ┌─┴─┐ *─┴─┐ ┌─┴─* ┌─┴─┐ ┌─┴─┐ ┌─┴─┐
	//    0   1 2   3 4   5 6   7 8   9 10  11
	//              ^^^
	//
	proof = buildSmallProof(3, 5, 12)
	subtreeRoot := func(i, j int) []byte {
		return bytesRoot(leafData[i*leafSize:j*leafSize], blake, leafSize)
	}
	manualProof := [][]byte{
		subtreeRoot(0, 2),
		subtreeRoot(2, 3),
		subtreeRoot(5, 6),
		subtreeRoot(6, 8),
		subtreeRoot(8, 12),
	}
	if !reflect.DeepEqual(proof, manualProof) {
		t.Error("BuildRangeProof constructed a proof that differs from manual proof")
	}

	// test a proof with precomputed inputs
	precalcRoots := [][]byte{
		bytesRoot(leafData[:len(leafData)/2], blake, leafSize),
		bytesRoot(leafData[len(leafData)/2:], blake, leafSize),
	}
	precalc := newPrecalcSubtreeHasher(precalcRoots, numLeaves/2, blake, NewReaderSubtreeHasher(bytes.NewReader(leafData), leafSize, blake))
	proof, err := BuildRangeProof(numLeaves-1, numLeaves, precalc)
	if err != nil {
		t.Fatal(err)
	}
	recalcProof, err := BuildRangeProof(numLeaves-1, numLeaves, NewReaderSubtreeHasher(bytes.NewReader(leafData), leafSize, blake))
	if !reflect.DeepEqual(proof, recalcProof) {
		t.Fatal("precalc failed")
	}
}

// TestBuildProofRangeEOF tests that BuildRangeProof behaves correctly in the
// presence of EOF errors.
func TestBuildProofRangeEOF(t *testing.T) {
	// setup proof parameters
	blake, _ := blake2b.New256(nil)
	leafData := make([]byte, 1<<22)
	const leafSize = 64
	numLeaves := len(leafData) / 64
	leafHashes := make([][]byte, numLeaves)
	for i := range leafHashes {
		leafHashes[i] = leafSum(blake, leafData[i*leafSize:][:leafSize])
	}

	// build a proof for the middle of the tree, but only supply half of the
	// leafData. This should trigger an io.ErrUnexpectedEOF when
	// BuildRangeProof tries to skip over the proof range.
	midl, midr := numLeaves/2-1, numLeaves/2+1

	// test with both ReaderSubtreeHasher and CachedSubtreeHasher
	shs := []SubtreeHasher{
		NewReaderSubtreeHasher(bytes.NewReader(leafData[:len(leafData)/2]), leafSize, blake),
		NewCachedSubtreeHasher(leafHashes[:len(leafHashes)/2], blake),
	}
	for _, sh := range shs {
		if _, err := BuildRangeProof(midl, midr, sh); err != io.ErrUnexpectedEOF {
			t.Fatal("expected io.ErrUnexpectedEOF, got", err)
		}
	}
}

// BenchmarkBuildRangeProof benchmarks the performance of BuildRangeProof for
// various proof ranges.
func BenchmarkBuildRangeProof(b *testing.B) {
	blake, _ := blake2b.New256(nil)
	leafData := fastrand.Bytes(1 << 22)
	const leafSize = 64
	numLeaves := len(leafData) / 64

	benchRange := func(start, end int) func(*testing.B) {
		return func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_, _ = BuildRangeProof(start, end, NewReaderSubtreeHasher(bytes.NewReader(leafData), leafSize, blake))
			}
		}
	}

	b.Run("single", benchRange(0, 1))
	b.Run("half", benchRange(0, numLeaves/2))
	b.Run("mid", benchRange(numLeaves/2, 1+numLeaves/2))
	b.Run("full", benchRange(0, numLeaves-1))
}

// BenchmarkBuildRangeProof benchmarks the performance of BuildRangeProof for
// various proof ranges when a subset of the roots have been precalculated.
func BenchmarkBuildRangeProofPrecalc(b *testing.B) {
	blake, _ := blake2b.New256(nil)
	leafData := fastrand.Bytes(1 << 22)
	const leafSize = 64
	numLeaves := len(leafData) / 64
	root := bytesRoot(leafData, blake, leafSize)

	verifyProof := func(start, end int, proof [][]byte) bool {
		lh := NewReaderLeafHasher(bytes.NewReader(leafData[start*leafSize:end*leafSize]), blake, leafSize)
		ok, err := VerifyRangeProof(lh, blake, start, end, proof, root)
		if err != nil {
			b.Fatal(err)
		}
		return ok
	}

	// precalculate nodes to depth 4
	precalcRoots := make([][]byte, 16)
	precalcSize := numLeaves / 16
	for i := range precalcRoots {
		precalcRoots[i] = bytesRoot(leafData[i*precalcSize*leafSize:][:precalcSize*leafSize], blake, leafSize)
	}

	benchRange := func(start, end int) func(*testing.B) {
		return func(b *testing.B) {
			precalc := newPrecalcSubtreeHasher(precalcRoots, precalcSize, blake, NewReaderSubtreeHasher(bytes.NewReader(leafData), leafSize, blake))
			b.ReportAllocs()
			proof, _ := BuildRangeProof(start, end, precalc)
			if !verifyProof(start, end, proof) {
				b.Fatal("precalculated roots are incorrect")
			}
			for i := 0; i < b.N; i++ {
				precalc = newPrecalcSubtreeHasher(precalcRoots, precalcSize, blake, NewReaderSubtreeHasher(bytes.NewReader(leafData), leafSize, blake))
				_, _ = BuildRangeProof(start, end, precalc)
			}
		}
	}

	b.Run("single", benchRange(numLeaves-1, numLeaves))
	b.Run("sixteenth", benchRange(numLeaves-numLeaves/16, numLeaves))
}

// BenchmarkVerifyRange benchmarks the performance of VerifyRangeProof
// for various proof ranges.
func BenchmarkVerifyRangeProof(b *testing.B) {
	blake, _ := blake2b.New256(nil)
	leafData := fastrand.Bytes(1 << 22)
	const leafSize = 64
	numLeaves := len(leafData) / 64
	root := bytesRoot(leafData, blake, leafSize)

	verifyProof := func(start, end int, proof [][]byte) bool {
		lh := NewReaderLeafHasher(bytes.NewReader(leafData[start*leafSize:end*leafSize]), blake, leafSize)
		ok, err := VerifyRangeProof(lh, blake, start, end, proof, root)
		if err != nil {
			b.Fatal(err)
		}
		return ok
	}

	benchRange := func(start, end int) func(*testing.B) {
		proof, _ := BuildRangeProof(start, end, NewReaderSubtreeHasher(bytes.NewReader(leafData), leafSize, blake))
		return func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_ = verifyProof(start, end, proof)
			}
		}
	}

	b.Run("single", benchRange(0, 1))
	b.Run("half", benchRange(0, numLeaves/2))
	b.Run("mid", benchRange(numLeaves/2, 1+numLeaves/2))
	b.Run("full", benchRange(0, numLeaves-1))
}
