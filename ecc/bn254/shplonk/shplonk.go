// Copyright 2020 Consensys Software Inc.
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

//cf https://eprint.iacr.org/2020/081.pdf

package shplonk

import (
	"errors"
	"hash"
	"math/big"

	"github.com/consensys/gnark-crypto/ecc"
	"github.com/consensys/gnark-crypto/ecc/bn254"
	"github.com/consensys/gnark-crypto/ecc/bn254/fr"
	"github.com/consensys/gnark-crypto/ecc/bn254/kzg"
	fiatshamir "github.com/consensys/gnark-crypto/fiat-shamir"
)

var (
	ErrInvalidNumberOfPoints = errors.New("number of digests should be equal to the number of points")
	ErrVerifyOpeningProof    = errors.New("can't verify batch opening proof")
)

// OpeningProof KZG proof for opening (fᵢ)_{i} at a different points (xᵢ)_{i}.
//
// implements io.ReaderFrom and io.WriterTo
type OpeningProof struct {

	// W = ∑ᵢ γⁱZ_{T\xᵢ}(f_i(X)-f(x_i)) where Z_{T} is the vanishing polynomial on the (xᵢ)_{i}
	W bn254.G1Affine

	// L(X)/(X-z) where L(X)=∑ᵢγⁱZ_{T\xᵢ}(f_i(X)-rᵢ) - Z_{T}W(X)
	WPrime bn254.G1Affine

	// (fᵢ(xᵢ))_{i}
	ClaimedValues []fr.Element
}

func BatchOpen(polynomials [][]fr.Element, digests []kzg.Digest, points []fr.Element, hf hash.Hash, pk kzg.ProvingKey, dataTranscript ...[]byte) (OpeningProof, error) {

	var res OpeningProof

	nbInstances := len(polynomials)
	if len(polynomials) != len(points) {
		return res, ErrInvalidNumberOfPoints
	}

	// transcript
	fs := fiatshamir.NewTranscript(hf, "gamma", "z")

	// derive γ
	gamma, err := deriveChallenge("gamma", points, digests, fs, dataTranscript...)
	if err != nil {
		return res, err
	}

	// compute the claimed evaluations
	maxSizePolys := len(polynomials[0])
	for i := 1; i < len(polynomials); i++ {
		if maxSizePolys < len(polynomials[i]) {
			maxSizePolys = len(polynomials[i])
		}
	}
	totalSize := maxSizePolys + len(points) - 1 // maxSizePolys+len(points)-2 is the max degree among the polynomials Z_{T\xᵢ}fᵢ

	bufTotalSize := make([]fr.Element, totalSize)
	bufMaxSizePolynomials := make([]fr.Element, maxSizePolys)
	f := make([]fr.Element, totalSize) // cf https://eprint.iacr.org/2020/081.pdf page 11 for notation
	bufPoints := make([]fr.Element, nbInstances-1)
	ztMinusXi := make([][]fr.Element, nbInstances)
	res.ClaimedValues = make([]fr.Element, nbInstances)
	var accGamma fr.Element
	accGamma.SetOne()

	for i := 0; i < nbInstances; i++ {

		res.ClaimedValues[i] = eval(polynomials[i], points[i])

		copy(bufPoints, points[:i])
		copy(bufPoints[i:], points[i+1:])
		ztMinusXi[i] = buildVanishingPoly(bufPoints)

		copy(bufMaxSizePolynomials, polynomials[i])
		bufMaxSizePolynomials[0].Sub(&bufMaxSizePolynomials[0], &res.ClaimedValues[i])
		bufTotalSize = mul(bufMaxSizePolynomials, ztMinusXi[i], bufTotalSize)
		bufTotalSize = mulByConstant(bufTotalSize, accGamma)
		for j := 0; j < len(bufTotalSize); j++ {
			f[j].Add(&f[j], &bufTotalSize[j])
		}

		accGamma.Mul(&accGamma, &gamma)
		setZero(bufMaxSizePolynomials)
	}

	zt := buildVanishingPoly(points)
	w := div(f, zt) // cf https://eprint.iacr.org/2020/081.pdf page 11 for notation page 11 for notation
	res.W, err = kzg.Commit(w, pk)
	if err != nil {
		return res, err
	}

	// derive z
	z, err := deriveChallenge("z", nil, []kzg.Digest{res.W}, fs)
	if err != nil {
		return res, err
	}

	// compute L = ∑ᵢγⁱZ_{T\xᵢ}(z)(fᵢ-rᵢ(z))-Z_{T}(z)W
	accGamma.SetOne()
	var gammaiZtMinusXi fr.Element
	l := make([]fr.Element, totalSize) // cf https://eprint.iacr.org/2020/081.pdf page 11 for notation page 11 for notation
	for i := 0; i < len(polynomials); i++ {

		zi := eval(ztMinusXi[i], z)
		gammaiZtMinusXi.Mul(&accGamma, &zi)
		copy(bufMaxSizePolynomials, polynomials[i])
		bufMaxSizePolynomials[0].Sub(&bufMaxSizePolynomials[0], &res.ClaimedValues[i])
		mulByConstant(bufMaxSizePolynomials, gammaiZtMinusXi)
		for j := 0; j < len(bufMaxSizePolynomials); j++ {
			l[j].Add(&l[j], &bufMaxSizePolynomials[j])
		}

		setZero(bufMaxSizePolynomials)
		accGamma.Mul(&accGamma, &gamma)
	}
	ztz := eval(zt, z)
	setZero(bufTotalSize)
	copy(bufTotalSize, w)
	mulByConstant(bufTotalSize, ztz)
	for i := 0; i < totalSize-maxSizePolys; i++ {
		l[totalSize-1-i].Neg(&bufTotalSize[totalSize-1-i])
	}
	for i := 0; i < maxSizePolys; i++ {
		l[i].Sub(&l[i], &bufTotalSize[i])
	}

	xMinusZ := buildVanishingPoly([]fr.Element{z})
	wPrime := div(l, xMinusZ)

	res.WPrime, err = kzg.Commit(wPrime, pk)
	if err != nil {
		return res, err
	}

	return res, nil
}

// BatchVerify uses proof to check that the commitments correctly open to proof.ClaimedValues
// at points. The order matters: the proof validates that the i-th commitment is correctly opened
// at the i-th point
// dataTranscript is some extra data that might be needed for Fiat Shamir, and is appended at the end
// of the original transcript.
func BatchVerify(proof OpeningProof, digests []kzg.Digest, points []fr.Element, hf hash.Hash, vk kzg.VerifyingKey, dataTranscript ...[]byte) error {

	if len(digests) != len(proof.ClaimedValues) {
		return ErrInvalidNumberOfPoints
	}
	if len(digests) != len(points) {
		return ErrInvalidNumberOfPoints
	}

	// transcript
	fs := fiatshamir.NewTranscript(hf, "gamma", "z")

	// derive γ
	gamma, err := deriveChallenge("gamma", points, digests, fs, dataTranscript...)
	if err != nil {
		return err
	}

	// derive z
	// TODO seems ok that z depend only on W, need to check that carefully
	z, err := deriveChallenge("z", nil, []kzg.Digest{proof.W}, fs)
	if err != nil {
		return err
	}

	// check that e(F + zW', [1]_{2})=e(W',[x]_{2})
	// where F = ∑ᵢγⁱZ_{T\xᵢ}[Com]_{i}-[∑ᵢγⁱZ_{T\xᵢ}(z)fᵢ(z)]_{1}-Z_{T}(z)[W]
	var sumGammaiZTminusXiFiz, tmp, accGamma fr.Element
	nbInstances := len(points)
	gammaiZTminusXiz := make([]fr.Element, nbInstances)
	accGamma.SetOne()
	bufPoints := make([]fr.Element, len(points)-1)
	for i := 0; i < len(points); i++ {

		copy(bufPoints, points[:i])
		copy(bufPoints[i:], points[i+1:])

		ztMinusXi := buildVanishingPoly(bufPoints)
		gammaiZTminusXiz[i] = eval(ztMinusXi, z)
		gammaiZTminusXiz[i].Mul(&accGamma, &gammaiZTminusXiz[i])

		tmp.Mul(&gammaiZTminusXiz[i], &proof.ClaimedValues[i])
		sumGammaiZTminusXiFiz.Add(&sumGammaiZTminusXiFiz, &tmp)

		accGamma.Mul(&accGamma, &gamma)
	}

	// ∑ᵢγⁱZ_{T\xᵢ}[Com]_{i}
	config := ecc.MultiExpConfig{}
	var sumGammaiZtMinusXiComi kzg.Digest
	_, err = sumGammaiZtMinusXiComi.MultiExp(digests, gammaiZTminusXiz, config)
	if err != nil {
		return err
	}

	var bufBigInt big.Int

	// [∑ᵢZ_{T\xᵢ}fᵢ(z)]_{1}
	var sumGammaiZTminusXiFizCom kzg.Digest
	var sumGammaiZTminusXiFizBigInt big.Int
	sumGammaiZTminusXiFiz.BigInt(&sumGammaiZTminusXiFizBigInt)
	sumGammaiZTminusXiFizCom.ScalarMultiplication(&vk.G1, &sumGammaiZTminusXiFizBigInt)

	// Z_{T}(z)[W]
	zt := buildVanishingPoly(points)
	ztz := eval(zt, z)
	var ztW kzg.Digest
	ztz.BigInt(&bufBigInt)
	ztW.ScalarMultiplication(&proof.W, &bufBigInt)

	// F = ∑ᵢγⁱZ_{T\xᵢ}[Com]_{i} - [∑ᵢ\gamma^{i}Z_{T\xᵢ}fᵢ(z)]_{1} - Z_{T}(z)[W]
	var f kzg.Digest
	f.Sub(&sumGammaiZtMinusXiComi, &sumGammaiZTminusXiFizCom).
		Sub(&f, &ztW)

	// F+zW'
	var zWPrime kzg.Digest
	z.BigInt(&bufBigInt)
	zWPrime.ScalarMultiplication(&proof.WPrime, &bufBigInt)
	f.Add(&f, &zWPrime)
	f.Neg(&f)

	// check that e(F+zW',[1]_{2})=e(W',[x]_{2})
	check, err := bn254.PairingCheckFixedQ(
		[]bn254.G1Affine{f, proof.WPrime},
		vk.Lines[:],
	)

	if !check {
		return ErrVerifyOpeningProof
	}

	return nil
}

// deriveChallenge derives a challenge using Fiat Shamir to polynomials.
// The arguments are added to the transcript in the order in which they are given.
func deriveChallenge(name string, points []fr.Element, digests []kzg.Digest, t *fiatshamir.Transcript, dataTranscript ...[]byte) (fr.Element, error) {

	// derive the challenge gamma, binded to the point and the commitments
	for i := range points {
		if err := t.Bind(name, points[i].Marshal()); err != nil {
			return fr.Element{}, err
		}
	}
	for i := range digests {
		if err := t.Bind(name, digests[i].Marshal()); err != nil {
			return fr.Element{}, err
		}
	}

	for i := 0; i < len(dataTranscript); i++ {
		if err := t.Bind(name, dataTranscript[i]); err != nil {
			return fr.Element{}, err
		}
	}

	gammaByte, err := t.ComputeChallenge(name)
	if err != nil {
		return fr.Element{}, err
	}
	var gamma fr.Element
	gamma.SetBytes(gammaByte)

	return gamma, nil
}

// ------------------------------
// utils

// sets f to zero
func setZero(f []fr.Element) {
	for i := 0; i < len(f); i++ {
		f[i].SetZero()
	}
}

func eval(f []fr.Element, x fr.Element) fr.Element {
	var y fr.Element
	for i := len(f) - 1; i >= 0; i-- {
		y.Mul(&y, &x).Add(&y, &f[i])
	}
	return y
}

// returns γ*f, re-using f
func mulByConstant(f []fr.Element, gamma fr.Element) []fr.Element {
	for i := 0; i < len(f); i++ {
		f[i].Mul(&f[i], &gamma)
	}
	return f
}

// computes f <- (x-a)*f
// memory of f is re used, need to pass a copy to not modify it
func multiplyLinearFactor(f []fr.Element, a fr.Element) []fr.Element {
	s := len(f)
	var tmp fr.Element
	f = append(f, fr.NewElement(0))
	f[s] = f[s-1]
	for i := s - 1; i >= 1; i-- {
		tmp.Mul(&f[i], &a)
		f[i].Sub(&f[i-1], &tmp)
	}
	f[0].Mul(&f[0], &a).Neg(&f[0])
	return f
}

// returns πᵢ(X-xᵢ)
func buildVanishingPoly(x []fr.Element) []fr.Element {
	res := make([]fr.Element, 1, len(x)+1)
	res[0].SetOne()
	for i := 0; i < len(x); i++ {
		res = multiplyLinearFactor(res, x[i])
	}
	return res
}

// returns f*g using naive multiplication
// deg(f)>>deg(g), deg(small) =~ 10 max
// buf is used as a buffer and should not be f or g
// f and g are not modified
func mul(f, g []fr.Element, buf []fr.Element) []fr.Element {

	sizeRes := len(f) + len(g) - 1
	if len(buf) < sizeRes {
		s := make([]fr.Element, sizeRes-len(buf))
		buf = append(buf, s...)
	}
	setZero(buf)

	var tmp fr.Element
	for i := 0; i < len(g); i++ {
		for j := 0; j < len(f); j++ {
			tmp.Mul(&f[j], &g[i])
			buf[j+i].Add(&buf[j+i], &tmp)
		}
	}
	return buf
}

// returns f/g (assuming g divides f)
// OK to not use fft if deg(g) is small
// g's leading coefficient is assumed to be 1
// f memory is re-used for the result, need to pass a copy to not modify it
func div(f, g []fr.Element) []fr.Element {
	sizef := len(f)
	sizeg := len(g)
	stop := sizeg - +1
	var t fr.Element
	for i := sizef - 2; i >= stop; i-- {
		for j := 0; j < sizeg-1; j++ {
			t.Mul(&f[i+1], &g[sizeg-2-j])
			f[i-j].Sub(&f[i-j], &t)
		}
	}
	return f[sizeg-1:]
}
