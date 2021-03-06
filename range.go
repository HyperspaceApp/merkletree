package merkletree

import (
	"bytes"
	"hash"
	"io"
	"io/ioutil"
)

// A SubtreeHasher calculates subtree roots in sequential order, for use with
// BuildRangeProof.
type SubtreeHasher interface {
	// NextSubtreeRoot returns the root of the next n leaves. If fewer than n
	// leaves are left in the tree, NextSubtreeRoot returns the root of those
	// leaves and nil. If no leaves are left, NextSubtreeRoot returns io.EOF.
	NextSubtreeRoot(n int) ([]byte, error)
	// Skip skips the next n leaves. If fewer than n leaves are left in the
	// tree, Skip returns io.ErrUnexpectedEOF. If exactly n leaves are left,
	// Skip returns nil (not io.EOF).
	Skip(n int) error
}

// ReaderSubtreeHasher implements SubtreeHasher by reading leaf data from an
// underlying stream.
type ReaderSubtreeHasher struct {
	r    io.Reader
	h    hash.Hash
	leaf []byte
}

// NextSubtreeRoot implements SubtreeHasher.
func (rsh *ReaderSubtreeHasher) NextSubtreeRoot(subtreeSize int) ([]byte, error) {
	tree := New(rsh.h)
	for i := 0; i < subtreeSize; i++ {
		n, err := io.ReadFull(rsh.r, rsh.leaf)
		if n > 0 {
			tree.Push(rsh.leaf[:n])
		}
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			break // reading a partial leaf is normal at the end of the stream
		} else if err != nil {
			return nil, err
		}
	}
	root := tree.Root()
	if root == nil {
		// we didn't read anything; return EOF to signal that there are no
		// more subtrees to hash.
		return nil, io.EOF
	}
	return root, nil
}

// Skip implements SubtreeHasher.
func (rsh *ReaderSubtreeHasher) Skip(n int) (err error) {
	skipSize := int64(len(rsh.leaf) * n)
	skipped, err := io.CopyN(ioutil.Discard, rsh.r, skipSize)
	if err == io.EOF || err == io.ErrUnexpectedEOF {
		if skipped == skipSize {
			return nil
		}
		return io.ErrUnexpectedEOF
	}
	return err
}

// NewReaderSubtreeHasher returns a new ReaderSubtreeHasher that reads leaf data from r.
func NewReaderSubtreeHasher(r io.Reader, leafSize int, h hash.Hash) *ReaderSubtreeHasher {
	return &ReaderSubtreeHasher{
		r:    r,
		h:    h,
		leaf: make([]byte, leafSize),
	}
}

// CachedSubtreeHasher implements SubtreeHasher using a set of precomputed
// leaf hashes.
type CachedSubtreeHasher struct {
	leafHashes [][]byte
	h          hash.Hash
}

// NextSubtreeRoot implements SubtreeHasher.
func (csh *CachedSubtreeHasher) NextSubtreeRoot(subtreeSize int) ([]byte, error) {
	if len(csh.leafHashes) == 0 {
		return nil, io.EOF
	}
	tree := New(csh.h)
	for i := 0; i < subtreeSize && len(csh.leafHashes) > 0; i++ {
		if err := tree.PushSubTree(0, csh.leafHashes[0]); err != nil {
			return nil, err
		}
		csh.leafHashes = csh.leafHashes[1:]
	}
	return tree.Root(), nil
}

// Skip implements SubtreeHasher.
func (csh *CachedSubtreeHasher) Skip(n int) error {
	if n > len(csh.leafHashes) {
		return io.ErrUnexpectedEOF
	}
	csh.leafHashes = csh.leafHashes[n:]
	return nil
}

// NewCachedSubtreeHasher creates a CachedSubtreeHasher using the specified
// leaf hashes and hash function.
func NewCachedSubtreeHasher(leafHashes [][]byte, h hash.Hash) *CachedSubtreeHasher {
	return &CachedSubtreeHasher{
		leafHashes: leafHashes,
		h:          h,
	}
}

// BuildRangeProof constructs a proof for the leaf range [proofStart,
// proofEnd) using the provided SubtreeHasher.
func BuildRangeProof(proofStart, proofEnd int, h SubtreeHasher) (proof [][]byte, err error) {
	if proofStart < 0 || proofStart > proofEnd || proofStart == proofEnd {
		panic("BuildRangeProof: illegal proof range")
	}

	// NOTE: this implementation is a bit magical. Essentially, the binary
	// property of Merkle trees allows us to determine which subtrees are
	// present in the proof just by looking at the binary representation of
	// the proofStart and proofEnd integers.
	//
	// As an example, imagine we are constructing the following proof:
	//
	//               ┌────────┴────────*
	//         ┌─────┴─────┐           │
	//      *──┴──┐     ┌──┴──*     ┌──┴──┐
	//    ┌─┴─┐ *─┴─┐ ┌─┴─* ┌─┴─┐ ┌─┴─┐ ┌─┴─┐
	//    0   1 2   3 4   5 6   7 8   9 10  11
	//              ^^^
	//
	// That is, proofStart = 3, proofEnd = 5, and there are 12 total leaves.
	// Each * represents a hash that should be included in the proof. But how
	// do we find these *s?
	//
	// We begin by examining the 1 bits in the binary representation of 3.
	// There are two 1 bits set, at exponents 1 and 0, which tells us that
	// there are two subtrees in the first half of the proof: one with 2^1
	// leaves, and one with 2^0 leaves. So we call NextSubtreeRoot(2) to get
	// the first proof hash, and NextSubtreeRoot(1) to get the second. The
	// order is important here: SubtreeHashers are stateful and proceed left-
	// to-right, so we should examine the bits in big-endian order to ensure
	// that we process larger subtrees first.
	//
	// The SubtreeHasher is now inside the proof range, so we call Skip to
	// proceed to the subtrees in the second half of the proof.
	//
	// We calculate the second half of the proof by examining bits again.
	// However, instead of looking at the 1 bits in proofStart, we look at the
	// 0 bits in proofEnd-1. All of the bits are 0 except for 2^2, so we call
	// NextSubtreeRoot on 2^0, 2^1, 2^3, and 2^4. Again, due to the nature of
	// the SubtreeHasher, order is important: we proceed in little-endian
	// order to ensure that we process smaller subtrees first. Finally, when
	// we attempt to call NextSubtreeRoot(2^4), it returns io.EOF, since we
	// are past the end of the tree, so the proof is complete.
	//
	// Why does this work? Well, it helps to realize that in a binary tree,
	// the bits of an leaf index describe the *path* from the root of the tree
	// to that leaf. For example: to reach leaf 2 in the above tree, start at
	// the top, then go left, left, right, left -- i.e. 0010. Thus, when we
	// want to construct a Merkle proof for a single leaf, we can use this
	// path to figure out which subtree hashes to include in the proof. But in
	// a multi-leaf proof, there are two paths: the path to proofStart, and
	// the path to proofEnd. So first we look at the path to proofStart, and
	// throw away all the hashes "to the right" of it, since we know those
	// will either be inside the proof range or part of the second half of the
	// proof; then, we look at the path to proofEnd, and do the opposite,
	// throwing away all the hashes "to the left" of it. That's why we look at
	// the 1 bits in proofStart, but the 0 bits in proofEnd. Visually:
	//
	//               ┌────────┴────────*          This is the Merkle proof for
	//         ┌─────┴─────*           │          leaf 3. Each "left-side" hash
	//      *──┴──┐     ┌──┴──┐     ┌──┴──┐       corresponds to a 1 bit in the
	//    ┌─┴─┐ *─┴─┐ ┌─┴─┐ ┌─┴─┐ ┌─┴─┐ ┌─┴─┐     binary string 1100.
	//    0   1 2   3 4   5 6   7 8   9 10  11
	//              ^
	//
	//               ┌────────┴────────*          This is the Merkle proof for
	//         *─────┴─────┐           │          leaf 4. Each "right-side" hash
	//      ┌──┴──┐     ┌──┴──*     ┌──┴──┐       corresponds to a 0 bit in the
	//    ┌─┴─┐ ┌─┴─┐ ┌─┴─* ┌─┴─┐ ┌─┴─┐ ┌─┴─┐     binary string 0010.
	//    0   1 2   3 4   5 6   7 8   9 10  11
	//                ^
	//
	// Combining the "left side" of the first proof with the "right side" of
	// the second yields the full range proof shown in the first diagram.

	// add proof hashes from leaves [0, proofStart)
	for i := 63; i >= 0; i-- {
		subtreeSize := 1 << uint64(i)
		if proofStart&subtreeSize != 0 {
			root, err := h.NextSubtreeRoot(subtreeSize)
			if err != nil {
				return nil, err
			}
			proof = append(proof, root)
		}
	}

	// skip leaves within proof range
	if err := h.Skip(proofEnd - proofStart); err != nil {
		return nil, err
	}

	// add proof hashes from proofEnd onward, stopping when NextSubtreeRoot
	// returns io.EOF.
	endMask := proofEnd - 1
	for i := 0; i < 64; i++ {
		subtreeSize := 1 << uint64(i)
		if endMask&subtreeSize == 0 {
			root, err := h.NextSubtreeRoot(int(subtreeSize))
			if err == io.EOF {
				break
			} else if err != nil {
				return nil, err
			}
			proof = append(proof, root)
		}
	}

	return proof, nil
}

// A LeafHasher returns the leaves of a Merkle tree in sequential order. When
// no more leaves are available, NextLeafHash must return io.EOF.
type LeafHasher interface {
	NextLeafHash() ([]byte, error)
}

// ReaderLeafHasher implements the LeafHasher interface by reading leaf data
// from the underlying stream.
type ReaderLeafHasher struct {
	r    io.Reader
	h    hash.Hash
	leaf []byte
}

// NextLeafHash implements LeafHasher.
func (rlh *ReaderLeafHasher) NextLeafHash() ([]byte, error) {
	n, err := io.ReadFull(rlh.r, rlh.leaf)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return nil, err
	} else if n == 0 {
		return nil, io.EOF
	}
	return leafSum(rlh.h, rlh.leaf[:n]), nil
}

// NewReaderLeafHasher creates a ReaderLeafHasher with the specified stream,
// hash, and leaf size.
func NewReaderLeafHasher(r io.Reader, h hash.Hash, leafSize int) *ReaderLeafHasher {
	return &ReaderLeafHasher{
		r:    r,
		h:    h,
		leaf: make([]byte, leafSize),
	}
}

// CachedLeafHasher implements the LeafHasher interface by returning
// precomputed leaf hashes.
type CachedLeafHasher struct {
	leafHashes [][]byte
}

// NextLeafHash implements LeafHasher.
func (clh *CachedLeafHasher) NextLeafHash() ([]byte, error) {
	if len(clh.leafHashes) == 0 {
		return nil, io.EOF
	}
	h := clh.leafHashes[0]
	clh.leafHashes = clh.leafHashes[1:]
	return h, nil
}

// NewCachedLeafHasher creates a CachedLeafHasher from a set of precomputed
// leaf hashes.
func NewCachedLeafHasher(leafHashes [][]byte) *CachedLeafHasher {
	return &CachedLeafHasher{
		leafHashes: leafHashes,
	}
}

// VerifyRangeProof verifies a proof produced by BuildRangeProof using leaf
// hashes produced by lh, which must contain only the leaf hashes within the
// proof range.
func VerifyRangeProof(lh LeafHasher, h hash.Hash, proofStart, proofEnd int, proof [][]byte, root []byte) (bool, error) {
	if proofStart < 0 || proofStart > proofEnd || proofStart == proofEnd {
		panic("VerifyRangeProof: illegal proof range")
	}

	// manually build a tree using the proof hashes
	tree := New(h)

	// add proof hashes up to proofStart
	for i := 63; i >= 0 && len(proof) > 0; i-- {
		subtreeSize := 1 << uint64(i)
		if proofStart&subtreeSize != 0 {
			if err := tree.PushSubTree(i, proof[0]); err != nil {
				// PushSubTree only returns an error if i is greater than the
				// current smallest subtree. Since the loop proceeds in
				// descending order, this should never happen.
				panic(err)
			}
			proof = proof[1:]
		}
	}

	// add leaf hashes
	for {
		leafHash, err := lh.NextLeafHash()
		if err == io.EOF {
			break
		} else if err != nil {
			return false, err
		}
		if err := tree.PushSubTree(0, leafHash); err != nil {
			panic(err)
		}
	}

	// add proof hashes after proofEnd
	endMask := proofEnd - 1
	for i := 0; i < 64 && len(proof) > 0; i++ {
		subtreeSize := 1 << uint64(i)
		if endMask&subtreeSize == 0 {
			if err := tree.PushSubTree(i, proof[0]); err != nil {
				// This *probably* should never happen, but just to guard
				// against adversarial inputs, return an error instead of
				// panicking.
				return false, err
			}
			proof = proof[1:]
		}
	}

	return bytes.Equal(tree.Root(), root), nil
}
