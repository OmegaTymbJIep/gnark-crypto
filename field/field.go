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

// Package field provides Golang code generation for efficient field arithmetic operations.
package field

import (
	"errors"
	"log"
	"math/big"

	"github.com/mmcloughlin/addchain"
	"github.com/mmcloughlin/addchain/acc"
	"github.com/mmcloughlin/addchain/acc/ast"
	"github.com/mmcloughlin/addchain/acc/ir"
	"github.com/mmcloughlin/addchain/acc/pass"
	"github.com/mmcloughlin/addchain/alg/ensemble"
	"github.com/mmcloughlin/addchain/alg/exec"
	"github.com/mmcloughlin/addchain/meta"
)

var (
	errUnsupportedModulus = errors.New("unsupported modulus. goff only works for prime modulus w/ size > 64bits")
	errParseModulus       = errors.New("can't parse modulus")
)

// Field precomputed values used in template for code generation of field element APIs
type Field struct {
	PackageName          string
	ElementName          string
	ModulusBig           *big.Int
	Modulus              string
	ModulusHex           string
	NbWords              int
	NbBits               int
	NbWordsLastIndex     int
	NbWordsIndexesNoZero []int
	NbWordsIndexesFull   []int
	Q                    []uint64
	QInverse             []uint64
	QMinusOneHalvedP     []uint64 // ((q-1) / 2 ) + 1
	ASM                  bool
	RSquare              []uint64
	One                  []uint64
	LegendreExponent     string // big.Int to base16 string
	NoCarry              bool
	NoCarrySquare        bool // used if NoCarry is set, but some op may overflow in square optimization
	SqrtQ3Mod4           bool
	SqrtAtkin            bool
	SqrtTonelliShanks    bool
	SqrtE                uint64
	SqrtS                []uint64

	SqrtAtkinExponent     string // big.Int to base16 string
	SqrtAtkinExponentData *addChainData

	SqrtSMinusOneOver2     string // big.Int to base16 string
	SqrtSMinusOneOver2Data *addChainData

	SqrtQ3Mod4Exponent     string // big.Int to base16 string
	SqrtQ3Mod4ExponentData *addChainData

	SqrtG []uint64 // NonResidue ^  SqrtR (montgomery form)

	NonResidue []uint64 // (montgomery form)
}

// NewField returns a data structure with needed informations to generate apis for field element
//
// See field/generator package
func NewField(packageName, elementName, modulus string) (*Field, error) {
	// parse modulus
	var bModulus big.Int
	if _, ok := bModulus.SetString(modulus, 10); !ok {
		return nil, errParseModulus
	}

	// field info
	F := &Field{
		PackageName: packageName,
		ElementName: elementName,
		Modulus:     modulus,
		ModulusHex:  bModulus.Text(16),
		ModulusBig:  new(big.Int).Set(&bModulus),
	}
	// pre compute field constants
	F.NbBits = bModulus.BitLen()
	F.NbWords = len(bModulus.Bits())
	if F.NbWords < 2 {
		return nil, errUnsupportedModulus
	}

	F.NbWordsLastIndex = F.NbWords - 1

	// set q from big int repr
	F.Q = toUint64Slice(&bModulus)
	_qHalved := big.NewInt(0)
	bOne := new(big.Int).SetUint64(1)
	_qHalved.Sub(&bModulus, bOne).Rsh(_qHalved, 1).Add(_qHalved, bOne)
	F.QMinusOneHalvedP = toUint64Slice(_qHalved, F.NbWords)

	//  setting qInverse
	_r := big.NewInt(1)
	_r.Lsh(_r, uint(F.NbWords)*64)
	_rInv := big.NewInt(1)
	_qInv := big.NewInt(0)
	extendedEuclideanAlgo(_r, &bModulus, _rInv, _qInv)
	_qInv.Mod(_qInv, _r)
	F.QInverse = toUint64Slice(_qInv, F.NbWords)

	//  rsquare
	_rSquare := big.NewInt(2)
	exponent := big.NewInt(int64(F.NbWords) * 64 * 2)
	_rSquare.Exp(_rSquare, exponent, &bModulus)
	F.RSquare = toUint64Slice(_rSquare, F.NbWords)

	var one big.Int
	one.SetUint64(1)
	one.Lsh(&one, uint(F.NbWords)*64).Mod(&one, &bModulus)
	F.One = toUint64Slice(&one, F.NbWords)

	// indexes (template helpers)
	F.NbWordsIndexesFull = make([]int, F.NbWords)
	F.NbWordsIndexesNoZero = make([]int, F.NbWords-1)
	for i := 0; i < F.NbWords; i++ {
		F.NbWordsIndexesFull[i] = i
		if i > 0 {
			F.NbWordsIndexesNoZero[i-1] = i
		}
	}

	// See https://hackmd.io/@zkteam/modular_multiplication
	// if the last word of the modulus is smaller or equal to B,
	// we can simplify the montgomery multiplication
	const B = (^uint64(0) >> 1) - 1
	F.NoCarry = (F.Q[len(F.Q)-1] <= B) && F.NbWords <= 12
	const BSquare = (^uint64(0) >> 2)
	F.NoCarrySquare = F.Q[len(F.Q)-1] <= BSquare

	// Legendre exponent (p-1)/2
	var legendreExponent big.Int
	legendreExponent.SetUint64(1)
	legendreExponent.Sub(&bModulus, &legendreExponent)
	legendreExponent.Rsh(&legendreExponent, 1)
	F.LegendreExponent = legendreExponent.Text(16)

	// Sqrt pre computes
	var qMod big.Int
	qMod.SetUint64(4)
	if qMod.Mod(&bModulus, &qMod).Cmp(new(big.Int).SetUint64(3)) == 0 {
		// q ≡ 3 (mod 4)
		// using  z ≡ ± x^((p+1)/4) (mod q)
		F.SqrtQ3Mod4 = true
		var sqrtExponent big.Int
		sqrtExponent.SetUint64(1)
		sqrtExponent.Add(&bModulus, &sqrtExponent)
		sqrtExponent.Rsh(&sqrtExponent, 2)
		F.SqrtQ3Mod4Exponent = sqrtExponent.Text(16)

		// add chain stuff
		F.SqrtQ3Mod4ExponentData = getAddChain(&sqrtExponent)

	} else {
		// q ≡ 1 (mod 4)
		qMod.SetUint64(8)
		if qMod.Mod(&bModulus, &qMod).Cmp(new(big.Int).SetUint64(5)) == 0 {
			// q ≡ 5 (mod 8)
			// use Atkin's algorithm
			// see modSqrt5Mod8Prime in math/big/int.go
			F.SqrtAtkin = true
			e := new(big.Int).Rsh(&bModulus, 3) // e = (q - 5) / 8
			F.SqrtAtkinExponent = e.Text(16)
			F.SqrtAtkinExponentData = getAddChain(e)
		} else {
			// use Tonelli-Shanks
			F.SqrtTonelliShanks = true

			// Write q-1 =2^e * s , s odd
			var s big.Int
			one.SetUint64(1)
			s.Sub(&bModulus, &one)

			e := s.TrailingZeroBits()
			s.Rsh(&s, e)
			F.SqrtE = uint64(e)
			F.SqrtS = toUint64Slice(&s)

			// find non residue
			var nonResidue big.Int
			nonResidue.SetInt64(2)
			one.SetUint64(1)
			for big.Jacobi(&nonResidue, &bModulus) != -1 {
				nonResidue.Add(&nonResidue, &one)
			}

			// g = nonresidue ^ s
			var g big.Int
			g.Exp(&nonResidue, &s, &bModulus)
			// store g in montgomery form
			g.Lsh(&g, uint(F.NbWords)*64).Mod(&g, &bModulus)
			F.SqrtG = toUint64Slice(&g, F.NbWords)

			// store non residue in montgomery form
			nonResidue.Lsh(&nonResidue, uint(F.NbWords)*64).Mod(&nonResidue, &bModulus)
			F.NonResidue = toUint64Slice(&nonResidue)

			// (s+1) /2
			s.Sub(&s, &one).Rsh(&s, 1)
			F.SqrtSMinusOneOver2 = s.Text(16)

			F.SqrtSMinusOneOver2Data = getAddChain(&s)
		}
	}

	// note: to simplify output files generated, we generated ASM code only for
	// moduli that meet the condition F.NoCarry
	// asm code generation for moduli with more than 6 words can be optimized further
	F.ASM = F.NoCarry && F.NbWords <= 12

	return F, nil
}

func toUint64Slice(b *big.Int, nbWords ...int) (s []uint64) {
	if len(nbWords) > 0 && nbWords[0] > len(b.Bits()) {
		s = make([]uint64, nbWords[0])
	} else {
		s = make([]uint64, len(b.Bits()))
	}

	for i, v := range b.Bits() {
		s[i] = (uint64)(v)
	}
	return
}

// https://en.wikipedia.org/wiki/Extended_Euclidean_algorithm
// r > q, modifies rinv and qinv such that rinv.r - qinv.q = 1
func extendedEuclideanAlgo(r, q, rInv, qInv *big.Int) {
	var s1, s2, t1, t2, qi, tmpMuls, riPlusOne, tmpMult, a, b big.Int
	t1.SetUint64(1)
	rInv.Set(big.NewInt(1))
	qInv.Set(big.NewInt(0))
	a.Set(r)
	b.Set(q)

	// r_i+1 = r_i-1 - q_i.r_i
	// s_i+1 = s_i-1 - q_i.s_i
	// t_i+1 = t_i-1 - q_i.s_i
	for b.Sign() > 0 {
		qi.Div(&a, &b)
		riPlusOne.Mod(&a, &b)

		tmpMuls.Mul(&s1, &qi)
		tmpMult.Mul(&t1, &qi)

		s2.Set(&s1)
		t2.Set(&t1)

		s1.Sub(rInv, &tmpMuls)
		t1.Sub(qInv, &tmpMult)
		rInv.Set(&s2)
		qInv.Set(&t2)

		a.Set(&b)
		b.Set(&riPlusOne)
	}
	qInv.Neg(qInv)
}

func getAddChain(n *big.Int) *addChainData {
	// Default ensemble of algorithms.
	algorithms := ensemble.Ensemble()

	// Use parallel executor.
	ex := exec.NewParallel()
	results := ex.Execute(n, algorithms)

	// Output best result.
	best := 0
	for i, r := range results {
		if r.Err != nil {
			log.Fatal(r.Err)
		}
		if len(results[i].Program) < len(results[best].Program) {
			best = i
		}
	}
	r := results[best]
	p, err := acc.Decompile(r.Program)
	if err != nil {
		log.Fatal(err)
	}
	chain, err := acc.Build(p)
	if err != nil {
		log.Fatal(err)
	}

	data, err := prepareAddChainData(chain)
	if err != nil {
		log.Fatal(err)
	}

	return data
}

// Data provided to templates.
type addChainData struct {
	// Chain is the addition chain as a list of integers.
	Chain addchain.Chain

	// Ops is the complete sequence of addition operations required to compute
	// the addition chain.
	Ops addchain.Program

	// Script is the condensed representation of the addition chain computation
	// in the "addition chain calculator" language.
	Script *ast.Chain

	// Program is the intermediate representation of the addition chain
	// computation. This representation is likely the most convenient for code
	// generation. It contains a sequence of add, double and shift (repeated
	// doubling) instructions required to compute the chain. Temporary variable
	// allocation has been performed and the list of required temporaries
	// populated.
	Program *ir.Program

	// Metadata about the addchain project and the specific release parameters.
	// Please use this to include a reference or citation back to the addchain
	// project in your generated output.
	Meta *meta.Properties
}

// PrepareData builds input template data for the given addition chain script.
func prepareAddChainData(s *ast.Chain) (*addChainData, error) {
	// Prepare template data.
	allocator := pass.Allocator{
		Input:  "x",
		Output: "z",
		Format: "t%d",
	}
	// Translate to IR.
	p, err := acc.Translate(s)
	if err != nil {
		return nil, err
	}

	// Apply processing passes: temporary variable allocation, and computing the
	// full addition chain sequence and operations.
	if err := pass.Exec(p, allocator, pass.Func(pass.Eval)); err != nil {
		return nil, err
	}

	return &addChainData{
		Chain:   p.Chain,
		Ops:     p.Program,
		Script:  s,
		Program: p,
		Meta:    meta.Meta,
	}, nil
}
