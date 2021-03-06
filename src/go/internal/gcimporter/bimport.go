// Copyright 2015 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package gcimporter

import (
	"encoding/binary"
	"fmt"
	"go/constant"
	"go/token"
	"go/types"
	"sort"
	"unicode"
	"unicode/utf8"
)

// BImportData imports a package from the serialized package data
// and returns the number of bytes consumed and a reference to the package.
// If data is obviously malformed, an error is returned but in
// general it is not recommended to call BImportData on untrusted data.
func BImportData(imports map[string]*types.Package, data []byte, path string) (int, *types.Package, error) {
	// determine low-level encoding format
	read := 0
	var format byte = 'm' // missing format
	if len(data) > 0 {
		format = data[0]
		data = data[1:]
		read++
	}
	if format != 'c' && format != 'd' {
		return read, nil, fmt.Errorf("invalid encoding format in export data: got %q; want 'c' or 'd'", format)
	}

	// --- generic export data ---

	p := importer{
		imports:     imports,
		data:        data,
		debugFormat: format == 'd',
		read:        read,
	}

	if v := p.string(); v != "v0" {
		return p.read, nil, fmt.Errorf("unknown version: %s", v)
	}

	// populate typList with predeclared "known" types
	p.typList = append(p.typList, predeclared...)

	// read package data
	// TODO(gri) clean this up
	i := p.tagOrIndex()
	if i != packageTag {
		panic(fmt.Sprintf("package tag expected, got %d", i))
	}
	name := p.string()
	if s := p.string(); s != "" {
		panic(fmt.Sprintf("empty path expected, got %s", s))
	}
	pkg := p.imports[path]
	if pkg == nil {
		pkg = types.NewPackage(path, name)
		p.imports[path] = pkg
	}
	p.pkgList = append(p.pkgList, pkg)

	if debug && p.pkgList[0] != pkg {
		panic("imported packaged not found in pkgList[0]")
	}

	// read compiler-specific flags
	p.string() // discard

	// read consts
	for i := p.int(); i > 0; i-- {
		name := p.string()
		typ := p.typ()
		val := p.value()
		p.declare(types.NewConst(token.NoPos, pkg, name, typ, val))
	}

	// read vars
	for i := p.int(); i > 0; i-- {
		name := p.string()
		typ := p.typ()
		p.declare(types.NewVar(token.NoPos, pkg, name, typ))
	}

	// read funcs
	for i := p.int(); i > 0; i-- {
		name := p.string()
		sig := p.typ().(*types.Signature)
		p.int() // read and discard index of inlined function body
		p.declare(types.NewFunc(token.NoPos, pkg, name, sig))
	}

	// read types
	for i := p.int(); i > 0; i-- {
		// name is parsed as part of named type and the
		// type object is added to scope via respective
		// named type
		_ = p.typ().(*types.Named)
	}

	// complete interfaces
	for _, typ := range p.typList {
		if it, ok := typ.(*types.Interface); ok {
			it.Complete()
		}
	}

	// record all referenced packages as imports
	list := append(([]*types.Package)(nil), p.pkgList[1:]...)
	sort.Sort(byPath(list))
	pkg.SetImports(list)

	// package was imported completely and without errors
	pkg.MarkComplete()

	return p.read, pkg, nil
}

type importer struct {
	imports map[string]*types.Package
	data    []byte
	pkgList []*types.Package
	typList []types.Type

	debugFormat bool
	read        int // bytes read
}

func (p *importer) declare(obj types.Object) {
	if alt := p.pkgList[0].Scope().Insert(obj); alt != nil {
		// This can only happen if we import a package a second time.
		panic(fmt.Sprintf("%s already declared", alt.Name()))
	}
}

func (p *importer) pkg() *types.Package {
	// if the package was seen before, i is its index (>= 0)
	i := p.tagOrIndex()
	if i >= 0 {
		return p.pkgList[i]
	}

	// otherwise, i is the package tag (< 0)
	if i != packageTag {
		panic(fmt.Sprintf("unexpected package tag %d", i))
	}

	// read package data
	name := p.string()
	path := p.string()

	// we should never see an empty package name
	if name == "" {
		panic("empty package name in import")
	}

	// we should never see an empty import path
	if path == "" {
		panic("empty import path")
	}

	// if the package was imported before, use that one; otherwise create a new one
	pkg := p.imports[path]
	if pkg == nil {
		pkg = types.NewPackage(path, name)
		p.imports[path] = pkg
	}
	p.pkgList = append(p.pkgList, pkg)

	return pkg
}

func (p *importer) record(t types.Type) {
	p.typList = append(p.typList, t)
}

// A dddSlice is a types.Type representing ...T parameters.
// It only appears for parameter types and does not escape
// the importer.
type dddSlice struct {
	elem types.Type
}

func (t *dddSlice) Underlying() types.Type { return t }
func (t *dddSlice) String() string         { return "..." + t.elem.String() }

func (p *importer) typ() types.Type {
	// if the type was seen before, i is its index (>= 0)
	i := p.tagOrIndex()
	if i >= 0 {
		return p.typList[i]
	}

	// otherwise, i is the type tag (< 0)
	switch i {
	case namedTag:
		// read type object
		name := p.string()
		tpkg := p.pkg()
		scope := tpkg.Scope()
		obj := scope.Lookup(name)

		// if the object doesn't exist yet, create and insert it
		if obj == nil {
			obj = types.NewTypeName(token.NoPos, tpkg, name, nil)
			scope.Insert(obj)
		}

		if _, ok := obj.(*types.TypeName); !ok {
			panic(fmt.Sprintf("pkg = %s, name = %s => %s", tpkg, name, obj))
		}

		// associate new named type with obj if it doesn't exist yet
		t0 := types.NewNamed(obj.(*types.TypeName), nil, nil)

		// but record the existing type, if any
		t := obj.Type().(*types.Named)
		p.record(t)

		// read underlying type
		t0.SetUnderlying(p.typ())

		// interfaces don't have associated methods
		if _, ok := t0.Underlying().(*types.Interface); ok {
			return t
		}

		// read associated methods
		for i := p.int(); i > 0; i-- {
			name := p.string()
			recv, _ := p.paramList() // TODO(gri) do we need a full param list for the receiver?
			params, isddd := p.paramList()
			result, _ := p.paramList()
			p.int() // read and discard index of inlined function body
			sig := types.NewSignature(recv.At(0), params, result, isddd)
			t0.AddMethod(types.NewFunc(token.NoPos, tpkg, name, sig))
		}

		return t

	case arrayTag:
		t := new(types.Array)
		p.record(t)

		n := p.int64()
		*t = *types.NewArray(p.typ(), n)
		return t

	case sliceTag:
		t := new(types.Slice)
		p.record(t)

		*t = *types.NewSlice(p.typ())
		return t

	case dddTag:
		t := new(dddSlice)
		p.record(t)

		t.elem = p.typ()
		return t

	case structTag:
		t := new(types.Struct)
		p.record(t)

		n := p.int()
		fields := make([]*types.Var, n)
		tags := make([]string, n)
		for i := range fields {
			fields[i] = p.field()
			tags[i] = p.string()
		}
		*t = *types.NewStruct(fields, tags)
		return t

	case pointerTag:
		t := new(types.Pointer)
		p.record(t)

		*t = *types.NewPointer(p.typ())
		return t

	case signatureTag:
		t := new(types.Signature)
		p.record(t)

		params, isddd := p.paramList()
		result, _ := p.paramList()
		*t = *types.NewSignature(nil, params, result, isddd)
		return t

	case interfaceTag:
		// Create a dummy entry in the type list. This is safe because we
		// cannot expect the interface type to appear in a cycle, as any
		// such cycle must contain a named type which would have been
		// first defined earlier.
		n := len(p.typList)
		p.record(nil)

		// no embedded interfaces with gc compiler
		if p.int() != 0 {
			panic("unexpected embedded interface")
		}

		// read methods
		methods := make([]*types.Func, p.int())
		for i := range methods {
			pkg, name := p.fieldName()
			params, isddd := p.paramList()
			result, _ := p.paramList()
			sig := types.NewSignature(nil, params, result, isddd)
			methods[i] = types.NewFunc(token.NoPos, pkg, name, sig)
		}

		t := types.NewInterface(methods, nil)
		p.typList[n] = t
		return t

	case mapTag:
		t := new(types.Map)
		p.record(t)

		key := p.typ()
		val := p.typ()
		*t = *types.NewMap(key, val)
		return t

	case chanTag:
		t := new(types.Chan)
		p.record(t)

		var dir types.ChanDir
		// tag values must match the constants in cmd/compile/internal/gc/go.go
		switch d := p.int(); d {
		case 1 /* Crecv */ :
			dir = types.RecvOnly
		case 2 /* Csend */ :
			dir = types.SendOnly
		case 3 /* Cboth */ :
			dir = types.SendRecv
		default:
			panic(fmt.Sprintf("unexpected channel dir %d", d))
		}
		val := p.typ()
		*t = *types.NewChan(dir, val)
		return t

	default:
		panic(fmt.Sprintf("unexpected type tag %d", i))
	}
}

func (p *importer) field() *types.Var {
	pkg, name := p.fieldName()
	typ := p.typ()

	anonymous := false
	if name == "" {
		// anonymous field - typ must be T or *T and T must be a type name
		switch typ := deref(typ).(type) {
		case *types.Basic: // basic types are named types
			name = typ.Name()
		case *types.Named:
			pkg = p.pkgList[0]
			name = typ.Obj().Name()
		default:
			panic("anonymous field expected")
		}
		anonymous = true
	}

	return types.NewField(token.NoPos, pkg, name, typ, anonymous)
}

func (p *importer) fieldName() (*types.Package, string) {
	name := p.string()
	if name == "" {
		return nil, "" // anonymous field
	}
	pkg := p.pkgList[0]
	if name == "?" || name != "_" && !exported(name) {
		if name == "?" {
			name = ""
		}
		pkg = p.pkg()
	}
	return pkg, name
}

func (p *importer) paramList() (*types.Tuple, bool) {
	n := p.int()
	if n == 0 {
		return nil, false
	}
	// negative length indicates unnamed parameters
	named := true
	if n < 0 {
		n = -n
		named = false
	}
	// n > 0
	params := make([]*types.Var, n)
	isddd := false
	for i := range params {
		params[i], isddd = p.param(named)
	}
	return types.NewTuple(params...), isddd
}

func (p *importer) param(named bool) (*types.Var, bool) {
	t := p.typ()
	td, isddd := t.(*dddSlice)
	if isddd {
		t = types.NewSlice(td.elem)
	}

	var name string
	if named {
		name = p.string()
		if name == "" {
			panic("expected named parameter")
		}
	}

	// read and discard compiler-specific info
	p.string()

	return types.NewVar(token.NoPos, nil, name, t), isddd
}

func exported(name string) bool {
	ch, _ := utf8.DecodeRuneInString(name)
	return unicode.IsUpper(ch)
}

func (p *importer) value() constant.Value {
	switch kind := constant.Kind(p.int()); kind {
	case falseTag:
		return constant.MakeBool(false)
	case trueTag:
		return constant.MakeBool(true)
	case int64Tag:
		return constant.MakeInt64(p.int64())
	case floatTag:
		return p.float()
	case complexTag:
		re := p.float()
		im := p.float()
		return constant.BinaryOp(re, token.ADD, constant.MakeImag(im))
	case stringTag:
		return constant.MakeString(p.string())
	default:
		panic(fmt.Sprintf("unexpected value kind %d", kind))
	}
}

func (p *importer) float() constant.Value {
	sign := p.int()
	if sign == 0 {
		return constant.MakeInt64(0)
	}

	exp := p.int()
	mant := []byte(p.string()) // big endian

	// remove leading 0's if any
	for len(mant) > 0 && mant[0] == 0 {
		mant = mant[1:]
	}

	// convert to little endian
	// TODO(gri) go/constant should have a more direct conversion function
	//           (e.g., once it supports a big.Float based implementation)
	for i, j := 0, len(mant)-1; i < j; i, j = i+1, j-1 {
		mant[i], mant[j] = mant[j], mant[i]
	}

	// adjust exponent (constant.MakeFromBytes creates an integer value,
	// but mant represents the mantissa bits such that 0.5 <= mant < 1.0)
	exp -= len(mant) << 3
	if len(mant) > 0 {
		for msd := mant[len(mant)-1]; msd&0x80 == 0; msd <<= 1 {
			exp++
		}
	}

	x := constant.MakeFromBytes(mant)
	switch {
	case exp < 0:
		d := constant.Shift(constant.MakeInt64(1), token.SHL, uint(-exp))
		x = constant.BinaryOp(x, token.QUO, d)
	case exp > 0:
		x = constant.Shift(x, token.SHL, uint(exp))
	}

	if sign < 0 {
		x = constant.UnaryOp(token.SUB, x, 0)
	}
	return x
}

// ----------------------------------------------------------------------------
// Low-level decoders

func (p *importer) tagOrIndex() int {
	if p.debugFormat {
		p.marker('t')
	}

	return int(p.rawInt64())
}

func (p *importer) int() int {
	return int(p.int64())
}

func (p *importer) int64() int64 {
	if p.debugFormat {
		p.marker('i')
	}

	return p.rawInt64()
}

func (p *importer) string() string {
	if p.debugFormat {
		p.marker('s')
	}

	var b []byte
	if n := int(p.rawInt64()); n > 0 {
		b = p.data[:n]
		p.data = p.data[n:]
		p.read += n
	}
	return string(b)
}

func (p *importer) marker(want byte) {
	if got := p.data[0]; got != want {
		panic(fmt.Sprintf("incorrect marker: got %c; want %c (pos = %d)", got, want, p.read))
	}
	p.data = p.data[1:]
	p.read++

	pos := p.read
	if n := int(p.rawInt64()); n != pos {
		panic(fmt.Sprintf("incorrect position: got %d; want %d", n, pos))
	}
}

// rawInt64 should only be used by low-level decoders
func (p *importer) rawInt64() int64 {
	i, n := binary.Varint(p.data)
	p.data = p.data[n:]
	p.read += n
	return i
}

// ----------------------------------------------------------------------------
// Export format

// Tags. Must be < 0.
const (
	// Packages
	packageTag = -(iota + 1)

	// Types
	namedTag
	arrayTag
	sliceTag
	dddTag
	structTag
	pointerTag
	signatureTag
	interfaceTag
	mapTag
	chanTag

	// Values
	falseTag
	trueTag
	int64Tag
	floatTag
	fractionTag // not used by gc
	complexTag
	stringTag
)

var predeclared = []types.Type{
	// basic types
	types.Typ[types.Bool],
	types.Typ[types.Int],
	types.Typ[types.Int8],
	types.Typ[types.Int16],
	types.Typ[types.Int32],
	types.Typ[types.Int64],
	types.Typ[types.Uint],
	types.Typ[types.Uint8],
	types.Typ[types.Uint16],
	types.Typ[types.Uint32],
	types.Typ[types.Uint64],
	types.Typ[types.Uintptr],
	types.Typ[types.Float32],
	types.Typ[types.Float64],
	types.Typ[types.Complex64],
	types.Typ[types.Complex128],
	types.Typ[types.String],

	// aliases
	types.Universe.Lookup("byte").Type(),
	types.Universe.Lookup("rune").Type(),

	// error
	types.Universe.Lookup("error").Type(),

	// untyped types
	types.Typ[types.UntypedBool],
	types.Typ[types.UntypedInt],
	types.Typ[types.UntypedRune],
	types.Typ[types.UntypedFloat],
	types.Typ[types.UntypedComplex],
	types.Typ[types.UntypedString],
	types.Typ[types.UntypedNil],

	// package unsafe
	types.Typ[types.UnsafePointer],
}
