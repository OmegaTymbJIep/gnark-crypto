// Copyright 2020 ConsenSys Software Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package fri

import (
	"errors"
	"fmt"
	"hash"
	"math/big"
	"math/bits"

	"github.com/consensys/gnark-crypto/accumulator/merkletree"
	"github.com/consensys/gnark-crypto/ecc"
	"github.com/consensys/gnark-crypto/ecc/bn254/fr"
	"github.com/consensys/gnark-crypto/ecc/bn254/fr/fft"
	fiatshamir "github.com/consensys/gnark-crypto/fiat-shamir"
)

var (
	ErrLowDegree            = errors.New("the fully folded polynomial in not of degree 1")
	ErrProximityTestFolding = errors.New("one round of interaction failed")
	ErrOddSize              = errors.New("the size should be even")
	ErrMerkleRoot           = errors.New("merkle roots of the opening and the proof of proximity don't coincide")
	ErrMerklePath           = errors.New("merkle path proof is wrong")
	ErrRangePosition        = errors.New("the asked opening position is out of range")
)

const rho = 2

var NbRounds = 1
var ErrorRate float32

// Digest commitment of a polynomial.
type Digest []byte

// merkleProof helper structure to build the merkle proof
// At each round, two contiguous values from the evaluated polynomial
// are queried. For one value, the full Merkle path will be provided.
// For the neighbor value, only the leaf is provided (so proofSet will
// be empty), since the Merkle path is the same as for the first value.
type partialMerkleProof struct {

	// Merkle root
	merkleRoot []byte

	// proofSet stores [leaf ∥ node_1 ∥ .. ∥ merkleRoot ], where the leaf is not
	// hashed.
	proofSet [][]byte

	// number of leaves of the tree.
	numLeaves uint64
}

// MerkleProof used to open a polynomial
type OpeningProof struct {

	// those fields are private since they are only needed for
	// the verification, which is abstracted in the VerifyOpening
	// method.
	merkleRoot []byte
	proofSet   [][]byte
	numLeaves  uint64
	index      uint64

	// ClaimedValue value of the leaf. This field is exported
	// because it's needed for protocols using polynomial commitment
	// schemes (to verify an algebraic relation).
	ClaimedValue fr.Element
}

// IOPP Interactive Oracle Proof of Proximity
type IOPP uint

const (
	// Multiplicative version of FRI, using the map x->x², on a
	// power of 2 subgroup of Fr^{*}.
	RADIX_2_FRI IOPP = iota
)

// round contains the data corresponding to a single round
// of fri.
// It consists of a list of interactions between the prover and the verifier,
// where each interaction contains a challenge provided by the verifier, as
// well as MerkleProofs for the queries of the verifier. The Merkle proofs
// correspond to the openings of the i-th folded polynomial at 2 points that
// belong to the same fiber of x -> x².
type round struct {

	// stores the interactions between the prover and the verifier.
	// Each interaction results in a set or merkle proofs, corresponding
	// to the queries of the verifier.
	interactions [][2]partialMerkleProof

	// evaluation stores the evaluation of the fully folded polynomial.
	// The verifier need to reconstruct the polynomial, and check that
	// it is low degree.
	evaluation []fr.Element
}

// ProofOfProximity proof of proximity, attesting that
// a function is d-close to a low degree polynomial.
//
// It is composed of a series of interactions, emulated with Fiat Shamir,
//
type ProofOfProximity struct {

	// ID unique ID attached to the proof of proximity. It's needed for
	// protocols using Fiat Shamir for instance, where challenges are derived
	// from the proof of proximity.
	ID []byte

	// round contains the data corresponding to a single round
	// of fri. There are NbRounds rounds of interactions.
	rounds []round
}

// Iopp interface that an iopp should implement
type Iopp interface {

	// BuildProofOfProximity creates a proof of proximity that p is d-close to a polynomial
	// of degree len(p). The proof is built non interactively using Fiat Shamir.
	BuildProofOfProximity(p []fr.Element) (ProofOfProximity, error)

	// VerifyProofOfProximity verifies the proof of proximity. It returns an error if the
	// verification fails.
	VerifyProofOfProximity(proof ProofOfProximity) error

	// Opens a polynomial at gⁱ where i = position.
	Open(p []fr.Element, position uint64) (OpeningProof, error)

	// Verifies the opening of a polynomial at gⁱ where i = position.
	VerifyOpening(position uint64, openingProof OpeningProof, pp ProofOfProximity) error
}

// GetRho returns the factor ρ = size_code_word/size_polynomial
func GetRho() int {
	return rho
}

// New creates a new IOPP capable to handle degree(size) polynomials.
func (iopp IOPP) New(size uint64, h hash.Hash) Iopp {
	switch iopp {
	case RADIX_2_FRI:
		return newRadixTwoFri(size, h)
	default:
		panic("iopp name is not recognized")
	}
}

// radixTwoFri empty structs implementing compressionFunction for
// the squaring function.
type radixTwoFri struct {

	// hash function that is used for Fiat Shamir and for committing to
	// the oracles.
	h hash.Hash

	// nbSteps number of interactions between the prover and the verifier
	nbSteps int

	// domain used to build the Reed Solomon code from the given polynomial.
	// The size of the domain is ρ*size_polynomial.
	domain *fft.Domain
}

func newRadixTwoFri(size uint64, h hash.Hash) radixTwoFri {

	var res radixTwoFri

	// computing the number of steps
	n := ecc.NextPowerOfTwo(size)
	nbSteps := bits.TrailingZeros(uint(n))
	res.nbSteps = nbSteps

	// extending the domain
	n = n * rho

	// building the domains
	res.domain = fft.NewDomain(n)

	// fmt.Printf("g = %s\n", res.domain.Generator.String())

	// hash function
	res.h = h

	return res
}

// finds i such that gⁱ = a
// TODO for the moment assume it exits and easily computable
func (s radixTwoFri) log(a, g fr.Element) int {
	var i int
	var _g fr.Element
	_g.SetOne()
	for i = 0; ; i++ {
		if _g.Equal(&a) {
			break
		}
		_g.Mul(&_g, &g)
	}
	return i
}

// convertOrderCanonical convert the index i, an entry in a
// sorted polynomial, to the corresponding entry in canonical
// representation. n is the size of the polynomial.
func convertSortedCanonical(i, n int) int {
	if i%2 == 0 {
		return i / 2
	} else {
		l := (n - 1 - i) / 2
		return n - 1 - l
	}
}

// convertCanonicalSorted convert the index i, an entry in a
// sorted polynomial, to the corresponding entry in canonical
// representation. n is the size of the polynomial.
func convertCanonicalSorted(i, n int) int {

	if i < n/2 {
		return 2 * i
	} else {
		l := n - (i + 1)
		l = 2 * l
		return n - l - 1
	}

}

// deriveQueriesPositions derives the indices of the oracle
// function that the verifier has to pick, in sorted form.
// * pos is the initial position, i.e. the logarithm of the first challenge
// * size is the size of the initial polynomial
// * The result is a slice of []int, where each entry is a tuple (iₖ), such that
// the verifier needs to evaluate ∑ₖ oracle(iₖ)xᵏ to build
// the folded function.
func (s radixTwoFri) deriveQueriesPositions(pos int, size int) []int {

	// res := make([]int, s.nbSteps+1)

	// //l := s.log(a, s.domain.Generator)
	// l := int(pos.Uint64())
	// n := int(s.domain.Cardinality)

	// // first we convert from canonical indexation to sorted indexation
	// for i := 0; i < s.nbSteps+1; i++ {

	// 	// canonical → sorted
	// 	if l < n/2 {
	// 		res[i] = 2 * l
	// 	} else {
	// 		res[i] = (n - 1) - 2*(n-1-l)
	// 		l = l - n/2
	// 	}
	// 	n = n >> 1
	// }

	_s := size / 2
	res := make([]int, s.nbSteps)
	res[0] = pos
	for i := 1; i < s.nbSteps; i++ {
		t := (res[i-1] - (res[i-1] % 2)) / 2
		res[i] = convertCanonicalSorted(t, _s)
		_s = _s / 2
	}

	return res
}

// sort orders the evaluation of a polynomial on a domain
// such that contiguous entries are in the same fiber:
// {q(g⁰), q(g^{n/2}), q(g¹), q(g^{1+n/2}),...,q(g^{n/2-1}), q(gⁿ⁻¹)}
func sort(evaluations []fr.Element) []fr.Element {
	q := make([]fr.Element, len(evaluations))
	n := len(evaluations) / 2
	for i := 0; i < n; i++ {
		q[2*i].Set(&evaluations[i])
		q[2*i+1].Set(&evaluations[i+n])
	}
	return q
}

// Opens a polynomial at gⁱ where i = position.
func (s radixTwoFri) Open(p []fr.Element, position uint64) (OpeningProof, error) {

	// check that position is in the correct range
	if position >= s.domain.Cardinality {
		return OpeningProof{}, ErrRangePosition
	}

	// put q in evaluation form
	q := make([]fr.Element, s.domain.Cardinality)
	copy(q, p)
	s.domain.FFT(q, fft.DIF)
	fft.BitReverse(q)

	// sort q to have fibers in contiguous entries. The goal is to have one
	// Merkle path for both openings of entries which are in the same fiber.
	q = sort(q)

	// build the Merkle proof, we the position is converted to fit the sorted polynomial
	pos := convertCanonicalSorted(int(position), len(q))

	tree := merkletree.New(s.h)
	err := tree.SetIndex(uint64(pos))
	if err != nil {
		return OpeningProof{}, err
	}
	for i := 0; i < len(q); i++ {
		tree.Push(q[i].Marshal())
	}
	var res OpeningProof
	res.merkleRoot, res.proofSet, res.index, res.numLeaves = tree.Prove()

	// set the claimed value, which is the first entry of the Merkle proof
	res.ClaimedValue.SetBytes(res.proofSet[0])

	return res, nil
}

func checkRoots(a, b []byte) error {

	if len(a) != len(b) {
		return ErrMerkleRoot
	}
	for i := 0; i < len(a); i++ {
		if a[i] != b[i] {
			return ErrMerkleRoot
		}
	}

	return nil
}

// Verifies the opening of a polynomial.
// * position the point at which the proof is opened (the point is gⁱ where i = position)
// * openingProof Merkle path proof
// * pp proof of proximity, needed because before opening Merkle path proof one should be sure that the
// committed values come from a polynomial. During the verification of the Merkle path proof, the root
// hash of the Merkle path is compared to the root hash of the first interaction of the proof of proximity,
// those should be equal, if not an error is raised.
func (s radixTwoFri) VerifyOpening(position uint64, openingProof OpeningProof, pp ProofOfProximity) error {

	// To query the Merkle path, we look at the first series of interactions, and check whether it's the point
	// at 'position' or its neighbor that contains the full Merkle path.
	var fullMerkleProof int
	if len(pp.rounds[0].interactions[0][0].proofSet) > len(pp.rounds[0].interactions[0][1].proofSet) {
		fullMerkleProof = 0
	} else {
		fullMerkleProof = 1
	}

	// check that the merkle roots coincide
	err := checkRoots(openingProof.merkleRoot, pp.rounds[0].interactions[0][fullMerkleProof].merkleRoot)
	if err != nil {
		return err
	}

	// convert position to the sorted version
	sizePoly := s.domain.Cardinality
	pos := convertCanonicalSorted(int(position), int(sizePoly))

	// check the Merkle proof
	res := merkletree.VerifyProof(s.h, openingProof.merkleRoot, openingProof.proofSet, uint64(pos), openingProof.numLeaves)
	if !res {
		return ErrMerklePath
	}
	return nil

}

// foldPolynomialLagrangeBasis folds a polynomial p, expressed in Lagrange basis.
//
// Fᵣ[X]/(Xⁿ-1) is a free module of rank 2 on Fᵣ[Y]/(Y^{n/2}-1). If
// p∈ Fᵣ[X]/(Xⁿ-1), expressed in Lagrange basis, the function finds the coordinates
// p₁, p₂ of p in Fᵣ[Y]/(Y^{n/2}-1), expressed in Lagrange basis. Finally, it computes
// p₁ + x*p₂ and returns it.
//
// * p is the polynomial to fold, in Lagrange basis, sorted like this: p = [p(1),p(-1),p(g),p(-g),p(g²),p(-g²),...]
// * g is a generator of the subgroup of Fᵣ^{*} of size len(p)
// * x is the folding challenge x, used to return p₁+x*p₂
func foldPolynomialLagrangeBasis(pSorted []fr.Element, gInv, x fr.Element) []fr.Element {

	// we have the following system
	// p₁(g²ⁱ)+gⁱp₂(g²ⁱ) = p(gⁱ)
	// p₁(g²ⁱ)-gⁱp₂(g²ⁱ) = p(-gⁱ)
	// we solve the system for p₁(g²ⁱ),p₂(g²ⁱ)
	s := len(pSorted)
	res := make([]fr.Element, s/2)

	var p1, p2, twoInv, acc fr.Element
	twoInv.SetUint64(2).Inverse(&twoInv)
	acc.SetOne()

	for i := 0; i < s/2; i++ {

		p1.Add(&pSorted[2*i], &pSorted[2*i+1])
		p2.Sub(&pSorted[2*i], &pSorted[2*i+1]).Mul(&p2, &acc)
		res[i].Mul(&p2, &x).Add(&res[i], &p1).Mul(&res[i], &twoInv)

		acc.Mul(&acc, &gInv)

	}

	return res
}

// buildProofOfProximitySingleRound generates a proof that a function, given as an oracle from
// the verifier point of view, is in fact δ-close to a polynomial.
func (s radixTwoFri) buildProofOfProximitySingleRound(salt fr.Element, p []fr.Element) (round, error) {

	// the proof will contain nbSteps interactions
	var res round
	res.interactions = make([][2]partialMerkleProof, s.nbSteps)

	// Fiat Shamir transcript to derive the challenges. The xᵢ are used to fold the
	// polynomials.
	// During the i-th round, the prover has a polynomial P of degree n. The verifier sends
	// xᵢ∈ Fᵣ to the prover. The prover expresses F in Fᵣ[X,Y]/<Y-X²> as
	// P₀(Y)+X P₁(Y) where P₀, P₁ are of degree n/2, and he then folds the polynomial
	// by replacing x by xᵢ.
	xis := make([]string, s.nbSteps+1)
	for i := 0; i < s.nbSteps; i++ {
		xis[i] = fmt.Sprintf("x%d", i)
	}
	xis[s.nbSteps] = "s0"
	fs := fiatshamir.NewTranscript(s.h, xis...)

	// the salt is binded to the first challenge, to ensure the challenges
	// are different at each round.
	fs.Bind(xis[0], salt.Marshal())

	// step 1 : fold the polynomial using the xi

	// evalsAtRound stores the list of the nbSteps polynomial evaluations, each evaluation
	// corresponds to the evaluation o the folded polynomial at round i.
	evalsAtRound := make([][]fr.Element, s.nbSteps)

	// evaluate p and sort the result
	_p := make([]fr.Element, s.domain.Cardinality)
	copy(_p, p)
	s.domain.FFT(_p, fft.DIF)
	fft.BitReverse(_p)

	// gInv inverse of the generator of the cyclic group of size the size of the polynomial.
	// The size of the cyclic group is ρ*s.domainSize, and not s.domainSize.
	var gInv fr.Element
	gInv.Set(&s.domain.GeneratorInv)

	for i := 0; i < s.nbSteps; i++ {

		evalsAtRound[i] = sort(_p)
		// printVector(fmt.Sprintf("[%d]", i), evalsAtRound[i])
		// in the first round, tamper the evaluation
		// if i == 0 {
		// 	delta := int(ErrorRate * float32(s.domain[0].Cardinality))
		// 	// delta := 1
		// 	for k := 0; k < delta; k++ {
		// 		pos := rand.Intn(int(s.domain[0].Cardinality))
		// 		evalsAtRound[0][pos].SetRandom()
		// 	}
		// }

		// compute the root hash, needed to derive xi
		t := merkletree.New(s.h)
		for k := 0; k < len(_p); k++ {
			t.Push(evalsAtRound[i][k].Marshal())
		}
		rh := t.Root()
		err := fs.Bind(xis[i], rh)
		if err != nil {
			return res, err
		}

		// derive the challenge
		bxi, err := fs.ComputeChallenge(xis[i])
		if err != nil {
			return res, err
		}
		var xi fr.Element
		xi.SetBytes(bxi)
		// fmt.Printf("x%d = %s\n", i, xi.String())

		// fold _p, reusing its memory
		_p = foldPolynomialLagrangeBasis(evalsAtRound[i], gInv, xi)

		// g <- g²
		gInv.Square(&gInv)

	}

	// last round, provide the evaluation. The fully folded polynomial is of size rho. It should
	// correspond to the evaluation of a polynomial of degree 1 on ρ points, so those points
	// are supposed to be on a line.
	res.evaluation = make([]fr.Element, rho)
	copy(res.evaluation, _p)
	// printVector("eval", res.evaluation)

	// step 2: provide the Merkle proofs of the queries

	// derive the verifier queries
	for i := 0; i < len(res.evaluation); i++ {
		err := fs.Bind(xis[s.nbSteps], res.evaluation[i].Marshal())
		if err != nil {
			return res, err
		}
	}
	binSeed, err := fs.ComputeChallenge(xis[s.nbSteps])
	if err != nil {
		return res, err
	}
	var bPos, bCardinality big.Int
	bPos.SetBytes(binSeed)
	bCardinality.SetUint64(s.domain.Cardinality)
	bPos.Mod(&bPos, &bCardinality)
	si := s.deriveQueriesPositions(int(bPos.Uint64()), int(s.domain.Cardinality))
	// fmt.Printf("[PROVER]   [")
	// for i := 0; i < len(si); i++ {
	// 	fmt.Printf("%d, ", si[i])
	// }
	// fmt.Println("]")

	for i := 0; i < s.nbSteps; i++ {

		// build proofs of queries at s[i]
		t := merkletree.New(s.h)
		err := t.SetIndex(uint64(si[i]))
		if err != nil {
			return res, err
		}
		for k := 0; k < len(evalsAtRound[i]); k++ {
			t.Push(evalsAtRound[i][k].Marshal())
		}
		mr, proofSet, _, numLeaves := t.Prove()

		// c denotes the entry that contains the full Merkle proof. The entry 1-c will
		// only contain 2 elements, which are the neighbor point, and the hash of the
		// first point. The remaining of the Merkle path is common to both the original
		// point and its neighbor.
		c := si[i] % 2
		res.interactions[i][c] = partialMerkleProof{mr, proofSet, numLeaves}
		res.interactions[i][1-c] = partialMerkleProof{
			mr,
			make([][]byte, 2),
			numLeaves,
		}
		// fmt.Printf("openings: [%s, %s]\n", evalsAtRound[i][0].String(), evalsAtRound[i][1].String())
		res.interactions[i][1-c].proofSet[0] = evalsAtRound[i][si[i]+1-2*c].Marshal()
		s.h.Reset()
		_, err = s.h.Write(res.interactions[i][c].proofSet[0])
		if err != nil {
			return res, err
		}
		res.interactions[i][1-c].proofSet[1] = s.h.Sum(nil)

	}

	return res, nil

}

// BuildProofOfProximity generates a proof that a function, given as an oracle from
// the verifier point of view, is in fact δ-close to a polynomial.
func (s radixTwoFri) BuildProofOfProximity(p []fr.Element) (ProofOfProximity, error) {

	// the proof will contain nbSteps interactions
	var proof ProofOfProximity
	proof.rounds = make([]round, NbRounds)

	var err error
	var salt, one fr.Element
	one.SetOne()
	for i := 0; i < NbRounds; i++ {
		proof.rounds[i], err = s.buildProofOfProximitySingleRound(salt, p)
		if err != nil {
			return proof, err
		}
		salt.Add(&salt, &one)
	}

	return proof, nil
}

// verifyProofOfProximitySingleRound verifies the proof of proximity. It returns an error if the
// verification fails.
func (s radixTwoFri) verifyProofOfProximitySingleRound(salt fr.Element, proof round) error {

	// Fiat Shamir transcript to derive the challenges
	xis := make([]string, s.nbSteps+1)
	for i := 0; i < s.nbSteps; i++ {
		xis[i] = fmt.Sprintf("x%d", i)
	}
	xis[s.nbSteps] = "s0"
	fs := fiatshamir.NewTranscript(s.h, xis...)
	xi := make([]fr.Element, s.nbSteps)

	// the salt is binded to the first challenge, to ensure the challenges
	// are different at each round.
	fs.Bind(xis[0], salt.Marshal())

	for i := 0; i < s.nbSteps; i++ {
		err := fs.Bind(xis[i], proof.interactions[i][0].merkleRoot)
		if err != nil {
			return err
		}
		bxi, err := fs.ComputeChallenge(xis[i])
		if err != nil {
			return err
		}
		xi[i].SetBytes(bxi)
	}

	// fmt.Printf("xi = [")
	// for i := 0; i < len(xi); i++ {
	// 	fmt.Printf("Fr(%s),", xi[i].String())
	// }
	// fmt.Println("]")

	// derive the verifier queries
	for i := 0; i < len(proof.evaluation); i++ {
		err := fs.Bind(xis[s.nbSteps], proof.evaluation[i].Marshal())
		if err != nil {
			return err
		}
	}
	binSeed, err := fs.ComputeChallenge(xis[s.nbSteps])
	if err != nil {
		return err
	}
	var bPos, bCardinality big.Int
	bPos.SetBytes(binSeed)
	bCardinality.SetUint64(s.domain.Cardinality)
	bPos.Mod(&bPos, &bCardinality)
	si := s.deriveQueriesPositions(int(bPos.Uint64()), int(s.domain.Cardinality))

	// for each round check the Merkle proof and the correctness of the folding

	// current size of the polynomial
	var twoInv, accGInv fr.Element
	twoInv.SetUint64(2).Inverse(&twoInv)
	currentSize := int(s.domain.Cardinality)
	accGInv.Set(&s.domain.GeneratorInv)
	for i := 0; i < s.nbSteps; i++ {

		// correctness of Merkle proof
		// c is the entry containing the full Merkle proof.
		c := si[i] % 2
		res := merkletree.VerifyProof(
			s.h,
			proof.interactions[i][c].merkleRoot,
			proof.interactions[i][c].proofSet,
			uint64(si[i]),
			proof.interactions[i][c].numLeaves,
		)
		if !res {
			return ErrMerklePath
		}

		// we verify the Merkle proof for the neighbor query, to do that we have
		// to pick the full Merkle proof of the first entry, stripped off of the leaf and
		// the first node. We replace the leaf and the first node by the leaf and the first
		// node of the partial Merkle proof, since the leaf and the first node of both proofs
		// are the only entries that differ.
		proofSet := make([][]byte, len(proof.interactions[i][c].proofSet))
		copy(proofSet[2:], proof.interactions[i][c].proofSet[2:])
		proofSet[0] = proof.interactions[i][1-c].proofSet[0]
		proofSet[1] = proof.interactions[i][1-c].proofSet[1]
		res = merkletree.VerifyProof(
			s.h,
			proof.interactions[i][1-c].merkleRoot,
			proofSet,
			uint64(si[i]+1-2*c),
			proof.interactions[i][1-c].numLeaves,
		)
		if !res {
			return ErrMerklePath
		}

		// correctness of the folding
		if i < s.nbSteps-1 {

			var fe, fo, l, r, fn fr.Element

			// l = P(gⁱ), r = P(g^{i+n/2})
			l.SetBytes(proof.interactions[i][0].proofSet[0])
			r.SetBytes(proof.interactions[i][1].proofSet[0])
			// fmt.Printf("%d (l,r) =[%s, %s]\n", i, l.String(), r.String())

			// (g^{si[i]}, g^{si[i]+1}) is the fiber of g^{2*si[i]}. The system to solve
			// (for P₀(g^{2si[i]}), P₀(g^{2si[i]}) ) is:
			// P(g^{si[i]}) = P₀(g^{2si[i]}) +  g^{si[i]/2}*P₀(g^{2si[i]})
			// P(g^{si[i]+1}) = P₀(g^{2si[i]}) -  g^{si[i]/2}*P₀(g^{2si[i]})
			bm := big.NewInt(int64(si[i] / 2))
			var ginv fr.Element
			ginv.Exp(accGInv, bm)
			fe.Add(&l, &r)                                      // P₁(g²ⁱ) (to be multiplied by 2⁻¹)
			fo.Sub(&l, &r).Mul(&fo, &ginv)                      // P₀(g²ⁱ) (to be multiplied by 2⁻¹)
			fo.Mul(&fo, &xi[i]).Add(&fo, &fe).Mul(&fo, &twoInv) // P₀(g²ⁱ) + xᵢ * P₁(g²ⁱ)

			fn.SetBytes(proof.interactions[i+1][si[i+1]%2].proofSet[0])
			// fmt.Printf("%d (fn,fo) = [%s %s]\n", i, fn.String(), fo.String())

			if !fo.Equal(&fn) {
				return ErrProximityTestFolding
			}

			// next inverse generator
			accGInv.Square(&accGInv)
		}

		// divide the size by 2
		currentSize = currentSize >> 1
	}

	// last transition
	var fe, fo, l, r, fn fr.Element

	l.SetBytes(proof.interactions[s.nbSteps-1][0].proofSet[0])
	r.SetBytes(proof.interactions[s.nbSteps-1][1].proofSet[0])
	// fmt.Printf("%d (l,r) = [%s, %s]\n", 2, l.String(), r.String())

	// fmt.Printf("ginv = %s\n", accGInv.String())
	// fmt.Printf("[VERIFIER] %d\n", si[s.nbSteps-1]-(si[s.nbSteps-1]%2))
	// m := convertSortedCanonical(si[s.nbSteps-1]-(si[s.nbSteps-1]%2), currentSize)
	// fmt.Printf("m = %d\n", m)
	// bm := big.NewInt(int64(m))
	_si := si[s.nbSteps-1] / 2
	// fmt.Printf("_si = %d\n", _si)
	accGInv.Exp(accGInv, big.NewInt(int64(_si)))
	// fmt.Printf("%d (l,r) = [%s, %s]\n", s.nbSteps-1, l.String(), r.String())
	fe.Add(&l, &r)                                                // P₁(g²ⁱ) (to be multiplied by 2⁻¹)
	fo.Sub(&l, &r).Mul(&fo, &accGInv)                             // P₀(g²ⁱ) (to be multiplied by 2⁻¹)
	fo.Mul(&fo, &xi[s.nbSteps-1]).Add(&fo, &fe).Mul(&fo, &twoInv) // P₀(g²ⁱ) + xᵢ * P₁(g²ⁱ)

	// the entry of the evaluation vector doesn't matter since they are supposed to be equal.
	// The equality of the entries is tested later.
	fn.Set(&proof.evaluation[0])
	// fmt.Printf("fn?? = %s\n", proof.evaluation[0].String())

	// fmt.Printf("%d %s %s\n", s.nbSteps-1, fn.String(), fo.String())
	if !fo.Equal(&fn) {
		return ErrProximityTestFolding
	}

	// Last step: the final evaluation should be the evaluation of a degree 0 polynomial,
	// so it must be constant.
	for i := 1; i < rho; i++ {
		if !proof.evaluation[i].Equal(&proof.evaluation[0]) {
			return ErrLowDegree
		}
	}

	return nil
}

// VerifyProofOfProximity verifies the proof, by checking each interaction one
// by one.
func (s radixTwoFri) VerifyProofOfProximity(proof ProofOfProximity) error {

	var salt, one fr.Element
	one.SetOne()
	for i := 0; i < NbRounds; i++ {
		err := s.verifyProofOfProximitySingleRound(salt, proof.rounds[i])
		if err != nil {
			return err
		}
		salt.Add(&salt, &one)
	}
	return nil

}

func printVector(name string, v []fr.Element) {

	fmt.Printf("%s = ", name)
	fmt.Printf("[")
	for i := 0; i < len(v); i++ {
		fmt.Printf("Fr(%s),", v[i].String())
	}
	fmt.Printf("]\n")

}
