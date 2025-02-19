// Copyright (c) 2013 The Go Authors. All rights reserved.
//
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file or at
// https://developers.google.com/open-source/licenses/bsd.

// Package lint contains a linter for Go source code.
package hint

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

const styleGuideBase = "http://golang.org/s/comments"

// A Linter lints Go source code.
type Linter struct {
}

// Problem represents a problem in some source code.
type Problem struct {
	File       string         // name of the sourcefile
	Position   token.Position // position in source file
	Text       string         // the prose that describes the problem
	Link       string         // (optional) the link to the style guide for the problem
	Confidence float64        // a value in (0,1] estimating the confidence in this problem's correctness
	LineText   string         // the source line
	Category   string         // a short name for the general category of the problem
}

func (p *Problem) String() string {
	if p.Link != "" {
		return p.Text + "\n\n" + p.Link
	}
	return p.Text
}

// Lint lints src.
func (l *Linter) Lint(filename string, config *Config, src []byte) ([]Problem, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "", src, parser.ParseComments)
	if err != nil {
		return nil, err
	}
	return (&file{fset: fset, f: f, src: src, filename: filename, config: config}).lint(), nil
}

// file represents a file being linted.
type file struct {
	fset     *token.FileSet
	f        *ast.File
	src      []byte
	filename string

	// sortable is the set of types in the file that implement sort.Interface.
	sortable map[string]bool
	// main is whether this file is in a "main" package.
	main bool

	problems []Problem

	config *Config
}

func (f *file) isTest() bool { return strings.HasSuffix(f.filename, "_test.go") }

func (f *file) lint() []Problem {
	if f.config == nil {
		f.config = NewDefaultConfig()
	}

	f.scanSortable()
	f.main = f.isMain()

	if f.config.Package {
		f.lintPackageComment()
	}

	if f.config.Imports {
		f.lintImports()
		f.lintBlankImports()
	}

	if f.config.Exported {
		f.lintExported(f.config.PackagePrefixNames)
	}
	if f.config.Names {
		f.lintNames()
	}

	if f.config.VarDecls {
		f.lintVarDecls()
	}

	if f.config.Elses {
		f.lintElses()
	}

	f.lintRanges()

	f.lintErrorf()
	f.lintErrors()
	f.lintErrorStrings()

	if f.config.UseThis {
		f.lintReceiverThis()
	} else {
		f.lintReceiverNames()
	}

	f.lintIncDec()
	if f.config.MakeSlice {
		f.lintMakeSlice()
	}
	if f.config.ErrorReturn {
		f.lintErrorReturn()
	}

	if f.config.IgnoredReturn {
		f.lintIgnoredReturn()
	}

	if f.config.NamedReturn {
		f.lintNamedReturn()
	}

	return f.problems
}

type link string
type category string

// The variadic arguments may start with link and category types,
// and must end with a format string and any arguments.
func (f *file) errorf(n ast.Node, confidence float64, args ...interface{}) {
	if confidence < f.config.MinConfidence {
		return
	}

	p := f.fset.Position(n.Pos())
	problem := Problem{
		File:       f.filename,
		Position:   p,
		Confidence: confidence,
		LineText:   srcLine(f.src, p),
	}

argLoop:
	for len(args) > 1 { // always leave at least the format string in args
		switch v := args[0].(type) {
		case link:
			problem.Link = string(v)
		case category:
			problem.Category = string(v)
		default:
			break argLoop
		}
		args = args[1:]
	}

	problem.Text = fmt.Sprintf(args[0].(string), args[1:]...)

	f.problems = append(f.problems, problem)
}

func (f *file) scanSortable() {
	f.sortable = make(map[string]bool)

	// bitfield for which methods exist on each type.
	const (
		Len = 1 << iota
		Less
		Swap
	)
	nmap := map[string]int{"Len": Len, "Less": Less, "Swap": Swap}
	has := make(map[string]int)
	f.walk(func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Recv == nil {
			return true
		}
		// TODO(dsymonds): We could check the signature to be more precise.
		recv := receiverType(fn)
		if i, ok := nmap[fn.Name.Name]; ok {
			has[recv] |= i
		}
		return false
	})
	for typ, ms := range has {
		if ms == Len|Less|Swap {
			f.sortable[typ] = true
		}
	}
}

func (f *file) isMain() bool {
	if f.f.Name.Name == "main" {
		return true
	}
	return false
}

// lintPackageComment checks package comments. It complains if
// there is no package comment, or if it is not of the right form.
// This has a notable false positive in that a package comment
// could rightfully appear in a different file of the same package,
// but that's not easy to fix since this linter is file-oriented.
func (f *file) lintPackageComment() {
	if f.isTest() {
		return
	}

	const ref = styleGuideBase + "#Package_Comments"
	if f.f.Doc == nil {
		f.errorf(f.f, 0.2, link(ref), category("comments"), "should have a package comment, unless it's in another file for this package")
		return
	}
	s := f.f.Doc.Text()
	prefix := "Package " + f.f.Name.Name + " "
	if ts := strings.TrimLeft(s, " \t"); ts != s {
		f.errorf(f.f.Doc, 1, link(ref), category("comments"), "package comment should not have leading space")
		s = ts
	}
	// Only non-main packages need to keep to this form.
	if f.f.Name.Name != "main" && !strings.HasPrefix(s, prefix) {
		f.errorf(f.f.Doc, 1, link(ref), category("comments"), `package comment should be of the form "%s..."`, prefix)
	}
}

// lintBlankImports complains if a non-main package has blank imports that are
// not documented.
func (f *file) lintBlankImports() {
	// In package main and in tests, we don't complain about blank imports.
	if f.main || f.isTest() {
		return
	}

	// The first element of each contiguous group of blank imports should have
	// an explanatory comment of some kind.
	for i, imp := range f.f.Imports {
		pos := f.fset.Position(imp.Pos())

		if !isBlank(imp.Name) {
			continue // Ignore non-blank imports.
		}
		if i > 0 {
			prev := f.f.Imports[i-1]
			prevPos := f.fset.Position(prev.Pos())
			if isBlank(prev.Name) && prevPos.Line+1 == pos.Line {
				continue // A subsequent blank in a group.
			}
		}

		// This is the first blank import of a group.
		if imp.Doc == nil && imp.Comment == nil {
			ref := ""
			f.errorf(imp, 1, link(ref), category("imports"), "a blank import should be only in a main or test package, or have a comment justifying it")
		}
	}
}

// lintImports examines import blocks.
func (f *file) lintImports() {

	for _, is := range f.f.Imports {
		if is.Name != nil && is.Name.Name == "." && !f.isTest() {
			f.errorf(is, 1, link(styleGuideBase+"#Import_Dot"), category("imports"), "should not use dot imports")
		}
	}
}

const docCommentsLink = styleGuideBase + "#Doc_Comments"

// lintExported examines the exported names.
// It complains if any required doc comments are missing,
// or if they are not of the right form. The exact rules are in
// lintFuncDoc, lintTypeDoc and lintValueSpecDoc; this function
// also tracks the GenDecl structure being traversed to permit
// doc comments for constants to be on top of the const block.
// It also complains if the names stutter when combined with
// the package name.
func (f *file) lintExported(allowPackagePrefix bool) {
	if f.isTest() {
		return
	}

	var lastGen *ast.GenDecl // last GenDecl entered.

	// Set of GenDecls that have already had missing comments flagged.
	genDeclMissingComments := make(map[*ast.GenDecl]bool)

	f.walk(func(node ast.Node) bool {
		switch v := node.(type) {
		case *ast.GenDecl:
			if v.Tok == token.IMPORT {
				return false
			}
			// token.CONST, token.TYPE or token.VAR
			lastGen = v
			return true
		case *ast.FuncDecl:
			f.lintFuncDoc(v)
			thing := "func"
			if v.Recv != nil {
				thing = "method"
			}

			if !allowPackagePrefix {
				f.checkStutter(v.Name, thing)
			}
			// Don't proceed inside funcs.
			return false
		case *ast.TypeSpec:
			// inside a GenDecl, which usually has the doc
			doc := v.Doc
			if doc == nil {
				doc = lastGen.Doc
			}
			f.lintTypeDoc(v, doc)

			if !allowPackagePrefix {
				f.checkStutter(v.Name, "type")
			}
			// Don't proceed inside types.
			return false
		case *ast.ValueSpec:
			f.lintValueSpecDoc(v, lastGen, genDeclMissingComments)
			return false
		}
		return true
	})
}

var allCapsRE = regexp.MustCompile(`^[A-Z0-9_]+$`)

// lintNames examines all names in the file.
// It complains if any use underscores or incorrect known initialisms.
func (f *file) lintNames() {
	// Package names need slightly different handling than other names.
	// TODO: make it optional or warning
	if f.config.PackageUnderscore && strings.Contains(f.f.Name.Name, "_") && !strings.HasSuffix(f.f.Name.Name, "_test") {
		f.errorf(f.f, 1, link("http://golang.org/doc/effective_go.html#package-names"), category("naming"), "don't use an underscore in package name")
	}

	check := func(id *ast.Ident, thing string) {
		if id.Name == "_" {
			return
		}

		// Handle two common styles from other languages that don't belong in Go.
		if len(id.Name) >= 5 && allCapsRE.MatchString(id.Name) && strings.Contains(id.Name, "_") {
			// TODO: make it optional
			f.errorf(id, 0.6, link(styleGuideBase+"#Mixed_Caps"), category("naming"), "don't use ALL_CAPS in Go names; use CamelCase")
			return
		}
		if len(id.Name) > 2 && id.Name[0] == 'k' && id.Name[1] >= 'A' && id.Name[1] <= 'Z' {
			should := string(id.Name[1]+'a'-'A') + id.Name[2:]
			// TODO: why? make it optional?
			f.errorf(id, 0.6, link(styleGuideBase+"#Mixed_Caps"), category("naming"), "don't use leading k in Go names; %s %s should be %s", thing, id.Name, should)
		}

		should := f.fixName(id.Name)
		if id.Name == should {
			return
		}
		if len(id.Name) > 2 && strings.Contains(id.Name[1:], "_") {
			f.errorf(id, 0.8, link("http://golang.org/doc/effective_go.html#mixed-caps"), category("naming"), "don't use underscores in Go names; %s %s should be %s", thing, id.Name, should)
			return
		}
		f.errorf(id, 0.8, link(styleGuideBase+"#Initialisms"), category("naming"), "%s %s should be %s", thing, id.Name, should)
	}
	checkList := func(fl *ast.FieldList, thing string) {
		if fl == nil {
			return
		}
		for _, f := range fl.List {
			for _, id := range f.Names {
				check(id, thing)
			}
		}
	}
	f.walk(func(node ast.Node) bool {
		switch v := node.(type) {
		case *ast.AssignStmt:
			if v.Tok == token.ASSIGN {
				return true
			}
			for _, exp := range v.Lhs {
				if id, ok := exp.(*ast.Ident); ok {
					check(id, "var")
				}
			}
		case *ast.FuncDecl:
			if f.isTest() && (strings.HasPrefix(v.Name.Name, "Example") || strings.HasPrefix(v.Name.Name, "Test") || strings.HasPrefix(v.Name.Name, "Benchmark")) {
				return true
			}

			check(v.Name, "func")

			thing := "func"
			if v.Recv != nil {
				thing = "method"
			}
			checkList(v.Type.Params, thing+" parameter")
			checkList(v.Type.Results, thing+" result")
		case *ast.GenDecl:
			if v.Tok == token.IMPORT {
				return true
			}
			var thing string
			switch v.Tok {
			case token.CONST:
				thing = "const"
			case token.TYPE:
				thing = "type"
			case token.VAR:
				thing = "var"
			}
			for _, spec := range v.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					check(s.Name, thing)
				case *ast.ValueSpec:
					for _, id := range s.Names {
						check(id, thing)
					}
				}
			}
		case *ast.InterfaceType:
			// Do not check interface method names.
			// They are often constrainted by the method names of concrete types.
			for _, x := range v.Methods.List {
				ft, ok := x.Type.(*ast.FuncType)
				if !ok { // might be an embedded interface name
					continue
				}
				checkList(ft.Params, "interface method parameter")
				checkList(ft.Results, "interface method result")
			}
		case *ast.RangeStmt:
			if v.Tok == token.ASSIGN {
				return true
			}
			if id, ok := v.Key.(*ast.Ident); ok {
				check(id, "range var")
			}
			if id, ok := v.Value.(*ast.Ident); ok {
				check(id, "range var")
			}
		case *ast.StructType:
			for _, f := range v.Fields.List {
				for _, id := range f.Names {
					check(id, "struct field")
				}
			}
		}
		return true
	})
}

// fixName returns a different name if it should be different.
func (f *file) fixName(name string) (should string) {
	// Fast path for simple cases: "_" and all lowercase.
	if name == "_" {
		return name
	}
	allLower := true
	for _, r := range name {
		if !unicode.IsLower(r) {
			allLower = false
			break
		}
	}
	if allLower {
		return name
	}

	// Split camelCase at any lower->upper transition, and split on underscores.
	// Check each word for common initialisms.
	runes := []rune(name)
	w, i := 0, 0 // index of start of word, scan
	for i+1 <= len(runes) {
		eow := false // whether we hit the end of a word
		if i+1 == len(runes) {
			eow = true
		} else if runes[i+1] == '_' {
			// underscore; shift the remainder forward over any run of underscores
			eow = true
			n := 1
			for i+n+1 < len(runes) && runes[i+n+1] == '_' {
				n++
			}
			copy(runes[i+1:], runes[i+n+1:])
			runes = runes[:len(runes)-n]
		} else if unicode.IsLower(runes[i]) && !unicode.IsLower(runes[i+1]) {
			// lower->non-lower
			eow = true
		}
		i++
		if !eow {
			continue
		}

		// [w,i) is a word.
		word := string(runes[w:i])
		// TODO: configure initialisms here
		if u := strings.ToUpper(word); f.config.Initialisms[u] {
			// Keep consistent case, which is lowercase only at the start.
			if w == 0 && unicode.IsLower(runes[w]) {
				u = strings.ToLower(u)
			}
			// All the common initialisms are ASCII,
			// so we can replace the bytes exactly.
			copy(runes[w:], []rune(u))
		} else if w > 0 && strings.ToLower(word) == word {
			// already all lowercase, and not the first word, so uppercase the first character.
			runes[w] = unicode.ToUpper(runes[w])
		}
		w = i
	}
	return string(runes)
}

// lintTypeDoc examines the doc comment on a type.
// It complains if they are missing from an exported type,
// or if they are not of the standard form.
func (f *file) lintTypeDoc(t *ast.TypeSpec, doc *ast.CommentGroup) {
	if !ast.IsExported(t.Name.Name) {
		return
	}
	if doc == nil {
		f.errorf(t, 1, link(docCommentsLink), category("comments"), "exported type %v should have comment or be unexported", t.Name)
		return
	}

	s := doc.Text()
	articles := [...]string{"A", "An", "The"}
	for _, a := range articles {
		if strings.HasPrefix(s, a+" ") {
			s = s[len(a)+1:]
			break
		}
	}
	if !strings.HasPrefix(s, t.Name.Name+" ") {
		// TODO: make it optional?
		f.errorf(doc, 1, link(docCommentsLink), category("comments"), `comment on exported type %v should be of the form "%v ..." (with optional leading article)`, t.Name, t.Name)
	}
}

var commonMethods = map[string]bool{
	"Error":     true,
	"Read":      true,
	"ServeHTTP": true,
	"String":    true,
	"Write":     true,
}

// lintFuncDoc examines doc comments on functions and methods.
// It complains if they are missing, or not of the right form.
// It has specific exclusions for well-known methods (see commonMethods above).
func (f *file) lintFuncDoc(fn *ast.FuncDecl) {
	if !ast.IsExported(fn.Name.Name) {
		// func is unexported
		return
	}
	kind := "function"
	name := fn.Name.Name
	if fn.Recv != nil {
		// method
		kind = "method"
		recv := receiverType(fn)
		if !ast.IsExported(recv) {
			// receiver is unexported
			return
		}
		if commonMethods[name] {
			return
		}
		switch name {
		case "Len", "Less", "Swap":
			if f.sortable[recv] {
				return
			}
		}
		name = recv + "." + name
	}
	if fn.Doc == nil {
		f.errorf(fn, 1, link(docCommentsLink), category("comments"), "exported %s %s should have comment or be unexported", kind, name)
		return
	}
	s := fn.Doc.Text()
	prefix := fn.Name.Name + " "
	if !strings.HasPrefix(s, prefix) {
		f.errorf(fn.Doc, 1, link(docCommentsLink), category("comments"), `comment on exported %s %s should be of the form "%s..."`, kind, name, prefix)
	}
}

// lintValueSpecDoc examines package-global variables and constants.
// It complains if they are not individually declared,
// or if they are not suitably documented in the right form (unless they are in a block that is commented).
func (f *file) lintValueSpecDoc(vs *ast.ValueSpec, gd *ast.GenDecl, genDeclMissingComments map[*ast.GenDecl]bool) {
	kind := "var"
	if gd.Tok == token.CONST {
		kind = "const"
	}

	if len(vs.Names) > 1 {
		// Check that none are exported except for the first.
		for _, n := range vs.Names[1:] {
			if ast.IsExported(n.Name) {
				f.errorf(vs, 1, category("comments"), "exported %s %s should have its own declaration", kind, n.Name)
				return
			}
		}
	}

	// Only one name.
	name := vs.Names[0].Name
	if !ast.IsExported(name) {
		return
	}

	if vs.Doc == nil {
		if gd.Doc == nil && !genDeclMissingComments[gd] {
			block := ""
			if kind == "const" && gd.Lparen.IsValid() {
				block = " (or a comment on this block)"
			}
			f.errorf(vs, 1, link(docCommentsLink), category("comments"), "exported %s %s should have comment%s or be unexported", kind, name, block)
			genDeclMissingComments[gd] = true
		}
		return
	}
	prefix := name + " "
	if !strings.HasPrefix(vs.Doc.Text(), prefix) {
		f.errorf(vs.Doc, 1, link(docCommentsLink), category("comments"), `comment on exported %s %s should be of the form "%s..."`, kind, name, prefix)
	}
}

func (f *file) checkStutter(id *ast.Ident, thing string) {
	pkg, name := f.f.Name.Name, id.Name
	if !ast.IsExported(name) {
		// unexported name
		return
	}
	// A name stutters if the package name is a strict prefix
	// and the next character of the name starts a new word.
	if len(name) <= len(pkg) {
		// name is too short to stutter.
		// This permits the name to be the same as the package name.
		return
	}
	if !strings.EqualFold(pkg, name[:len(pkg)]) {
		return
	}
	// We can assume the name is well-formed UTF-8.
	// If the next rune after the package name is uppercase or an underscore
	// the it's starting a new word and thus this name stutters.
	rem := name[len(pkg):]
	if next, _ := utf8.DecodeRuneInString(rem); next == '_' || unicode.IsUpper(next) {
		f.errorf(id, 0.8, category("naming"), "%s name will be used as %s.%s by other packages, and that stutters; consider calling this %s", thing, pkg, name, rem)
	}
}

// zeroLiteral is a set of ast.BasicLit values that are zero values.
// It is not exhaustive.
var zeroLiteral = map[string]bool{
	"false": true, // bool
	// runes
	`'\x00'`: true,
	`'\000'`: true,
	// strings
	`""`: true,
	"``": true,
	// numerics
	"0":   true,
	"0.":  true,
	"0.0": true,
	"0i":  true,
}

// lintVarDecls examines variable declarations. It complains about declarations with
// redundant LHS types that can be inferred from the RHS.
func (f *file) lintVarDecls() {
	var lastGen *ast.GenDecl // last GenDecl entered.

	f.walk(func(node ast.Node) bool {
		switch v := node.(type) {
		case *ast.GenDecl:
			if v.Tok != token.CONST && v.Tok != token.VAR {
				return false
			}
			lastGen = v
			return true
		case *ast.ValueSpec:
			if lastGen.Tok == token.CONST {
				return false
			}
			if len(v.Names) > 1 || v.Type == nil || len(v.Values) == 0 {
				return false
			}
			rhs := v.Values[0]
			// An underscore var appears in a common idiom for compile-time interface satisfaction,
			// as in "var _ Interface = (*Concrete)(nil)".
			if isIdent(v.Names[0], "_") {
				return false
			}
			// If the RHS is a zero value, suggest dropping it.
			zero := false
			if lit, ok := rhs.(*ast.BasicLit); ok {
				zero = zeroLiteral[lit.Value]
			} else if isIdent(rhs, "nil") {
				zero = true
			}
			if zero {
				f.errorf(rhs, 0.9, category("zero-value"), "should drop = %s from declaration of var %s; it is the zero value", f.render(rhs), v.Names[0])
				return false
			}
			// If the LHS type is an interface, don't warn, since it is probably a
			// concrete type on the RHS. Note that our feeble lexical check here
			// will only pick up interface{} and other literal interface types;
			// that covers most of the cases we care to exclude right now.
			// TODO(dsymonds): Use typechecker to make this heuristic more accurate.
			if _, ok := v.Type.(*ast.InterfaceType); ok {
				return false
			}
			// If the RHS is an untyped const, only warn if the LHS type is its default type.
			if defType, ok := isUntypedConst(rhs); ok && !isIdent(v.Type, defType) {
				return false
			}
			f.errorf(v.Type, 0.8, category("type-inference"), "should omit type %s from declaration of var %s; it will be inferred from the right-hand side", f.render(v.Type), v.Names[0])
			return false
		}
		return true
	})
}

// lintElses examines else blocks. It complains about any else block whose if block ends in a return.
func (f *file) lintElses() {
	// We don't want to flag if { } else if { } else { } constructions.
	// They will appear as an IfStmt whose Else field is also an IfStmt.
	// Record such a node so we ignore it when we visit it.
	ignore := make(map[*ast.IfStmt]bool)

	f.walk(func(node ast.Node) bool {
		ifStmt, ok := node.(*ast.IfStmt)
		if !ok || ifStmt.Else == nil {
			return true
		}
		if ignore[ifStmt] {
			return true
		}
		if elseif, ok := ifStmt.Else.(*ast.IfStmt); ok {
			ignore[elseif] = true
			return true
		}
		if _, ok := ifStmt.Else.(*ast.BlockStmt); !ok {
			// only care about elses without conditions
			return true
		}
		if len(ifStmt.Body.List) == 0 {
			return true
		}
		shortDecl := false // does the if statement have a ":=" initialization statement?
		if ifStmt.Init != nil {
			if as, ok := ifStmt.Init.(*ast.AssignStmt); ok && as.Tok == token.DEFINE {
				shortDecl = true
			}
		}
		lastStmt := ifStmt.Body.List[len(ifStmt.Body.List)-1]
		if _, ok := lastStmt.(*ast.ReturnStmt); ok {
			extra := ""
			if shortDecl {
				extra = " (move short variable declaration to its own line if necessary)"
			}
			f.errorf(ifStmt.Else, 1, link(styleGuideBase+"#Indent_Error_Flow"), category("indent"), "if block ends with a return statement, so drop this else and outdent its block"+extra)
		}
		return true
	})
}

// lintRanges examines range clauses. It complains about redundant constructions.
func (f *file) lintRanges() {
	f.walk(func(node ast.Node) bool {
		rs, ok := node.(*ast.RangeStmt)
		if !ok {
			return true
		}
		if rs.Value == nil {
			// for x = range m { ... }
			return true // single var form
		}
		if !isIdent(rs.Value, "_") {
			// for ?, y = range m { ... }
			return true
		}

		f.errorf(rs.Value, 1, category("range-loop"), "should omit 2nd value from range; this loop is equivalent to `for %s %s range ...`", f.render(rs.Key), rs.Tok)
		return true
	})
}

// lintErrorf examines errors.New calls. It complains if its only argument is an fmt.Sprintf invocation.
func (f *file) lintErrorf() {
	f.walk(func(node ast.Node) bool {
		ce, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		if !isPkgDot(ce.Fun, "errors", "New") || len(ce.Args) != 1 {
			return true
		}
		arg := ce.Args[0]
		ce, ok = arg.(*ast.CallExpr)
		if !ok || !isPkgDot(ce.Fun, "fmt", "Sprintf") {
			return true
		}
		f.errorf(node, 1, category("errors"), "should replace errors.New(fmt.Sprintf(...)) with fmt.Errorf(...)")
		return true
	})
}

// lintErrors examines global error vars. It complains if they aren't named in the standard way.
func (f *file) lintErrors() {
	for _, decl := range f.f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.VAR {
			continue
		}
		for _, spec := range gd.Specs {
			spec := spec.(*ast.ValueSpec)
			if len(spec.Names) != 1 || len(spec.Values) != 1 {
				continue
			}
			ce, ok := spec.Values[0].(*ast.CallExpr)
			if !ok {
				continue
			}
			if !isPkgDot(ce.Fun, "errors", "New") && !isPkgDot(ce.Fun, "fmt", "Errorf") {
				continue
			}

			id := spec.Names[0]
			prefix := "err"
			if id.IsExported() {
				prefix = "Err"
			}
			if !strings.HasPrefix(id.Name, prefix) {
				f.errorf(id, 0.9, category("naming"), "error var %s should have name of the form %sFoo", id.Name, prefix)
			}
		}
	}
}

func lintCapAndPunct(s string) (isCap, isPunct bool) {
	first, firstN := utf8.DecodeRuneInString(s)
	last, _ := utf8.DecodeLastRuneInString(s)
	isPunct = last == '.' || last == ':' || last == '!'
	isCap = unicode.IsUpper(first)
	if isCap && len(s) > firstN {
		// Don't flag strings starting with something that looks like an initialism.
		if second, _ := utf8.DecodeRuneInString(s[firstN:]); unicode.IsUpper(second) {
			isCap = false
		}
	}
	return
}

// lintErrorStrings examines error strings. It complains if they are capitalized or end in punctuation.
func (f *file) lintErrorStrings() {
	f.walk(func(node ast.Node) bool {
		ce, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		if !isPkgDot(ce.Fun, "errors", "New") && !isPkgDot(ce.Fun, "fmt", "Errorf") {
			return true
		}
		if len(ce.Args) < 1 {
			return true
		}
		str, ok := ce.Args[0].(*ast.BasicLit)
		if !ok || str.Kind != token.STRING {
			return true
		}
		s, _ := strconv.Unquote(str.Value) // can assume well-formed Go
		if s == "" {
			return true
		}
		isCap, isPunct := lintCapAndPunct(s)
		var msg string
		switch {
		case isCap && isPunct:
			msg = "error strings should not be capitalized and should not end with punctuation"
		case isCap:
			msg = "error strings should not be capitalized"
		case isPunct:
			msg = "error strings should not end with punctuation"
		default:
			return true
		}
		// People use proper nouns and exported Go identifiers in error strings,
		// so decrease the confidence of warnings for capitalization.
		conf := 0.8
		if isCap {
			conf = 0.6
		}
		f.errorf(str, conf, link(styleGuideBase+"#Error_Strings"), category("errors"), msg)
		return true
	})
}

// lintReceiverThis examine reciever names. It argues on
// not "this" names
func (f *file) lintReceiverThis() {
	f.walk(func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Recv == nil {
			return true
		}
		names := fn.Recv.List[0].Names
		if len(names) < 1 {
			return true
		}
		name := names[0].Name
		if name != "this" {
			f.errorf(n, 1, category("naming"), `receiver name should be 'this'`)
		}
		return true
	})
}

// lintReceiverNames examines receiver names. It complains about inconsistent
// names used for the same type and names such as "this".
func (f *file) lintReceiverNames() {
	typeReceiver := map[string]string{}
	f.walk(func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Recv == nil {
			return true
		}
		names := fn.Recv.List[0].Names
		if len(names) < 1 {
			return true
		}
		name := names[0].Name
		const ref = styleGuideBase + "#Receiver_Names"
		if name == "_" {
			f.errorf(n, 1, link(ref), category("naming"), `receiver name should not be an underscore`)
			return true
		}
		if f.config.BadReceiverNames[name] {
			f.errorf(n, 1, link(ref), category("naming"), `receiver name should be a reflection of its identity; don't use generic names such as "me", "this", or "self"`)
			return true
		}
		recv := receiverType(fn)
		if prev, ok := typeReceiver[recv]; ok && prev != name {
			f.errorf(n, 1, link(ref), category("naming"), "receiver name %s should be consistent with previous receiver name %s for %s", name, prev, recv)
			return true
		}
		typeReceiver[recv] = name
		return true
	})
}

// lintIncDec examines statements that increment or decrement a variable.
// It complains if they don't use x++ or x--.
func (f *file) lintIncDec() {
	f.walk(func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok {
			return true
		}
		if len(as.Lhs) != 1 {
			return true
		}
		if !isOne(as.Rhs[0]) {
			return true
		}
		var suffix string
		switch as.Tok {
		case token.ADD_ASSIGN:
			suffix = "++"
		case token.SUB_ASSIGN:
			suffix = "--"
		default:
			return true
		}
		f.errorf(as, 0.8, category("unary-op"), "should replace %s with %s%s", f.render(as), f.render(as.Lhs[0]), suffix)
		return true
	})
}

// lintMakeSlice examines statements that declare and initialize a variable with make.
// It complains if they are constructing a zero element slice.
func (f *file) lintMakeSlice() {
	f.walk(func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok {
			return true
		}
		// Only want single var := assignment statements.
		if len(as.Lhs) != 1 || as.Tok != token.DEFINE {
			return true
		}
		ce, ok := as.Rhs[0].(*ast.CallExpr)
		if !ok {
			return true
		}
		// Check if ce is make([]T, 0).
		if !isIdent(ce.Fun, "make") || len(ce.Args) != 2 || !isZero(ce.Args[1]) {
			return true
		}
		at, ok := ce.Args[0].(*ast.ArrayType)
		if !ok || at.Len != nil {
			return true
		}
		f.errorf(as, 0.8, category("slice"), `can probably use "var %s %s" instead`, f.render(as.Lhs[0]), f.render(at))
		return true
	})
}

// lintErrorReturn examines function declarations that return an error.
// It complains if the error isn't the last parameter.
func (f *file) lintErrorReturn() {
	f.walk(func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Type.Results == nil {
			return true
		}
		ret := fn.Type.Results.List
		if len(ret) <= 1 {
			return true
		}
		// An error return parameter should be the last parameter.
		// Flag any error parameters found before the last.
		for _, r := range ret[:len(ret)-1] {
			if isIdent(r.Type, "error") {
				f.errorf(fn, 0.9, category("arg-order"), "error should be the last type when returning multiple items")
				break // only flag one
			}
		}
		return true
	})
}

// Check for ignored values returned from function calls. Ignored errors are special case.
// Errors can be ignored in 2 ways:
// 1. "silently" - when no acceptor is provided for returned error
// 2. "intentionally" - when acceptor for returned error is "_". Like: "_ := foo()"
func (f *file) lintIgnoredReturn() {
	f.walk(func(n ast.Node) bool {

		if expr, ok := n.(*ast.ExprStmt); ok && expr.X != nil {
			// process simple function call here, with no assignment.
			fn := extractFuncDecl(expr.X)
			errIndices := extractErrResultIndices(fn)
			if len(errIndices) > 0 {
				// check for ignored returned errors first
				f.errorf(expr, 1.0, category("result-ignore"), "function '%s' returns an error, it should not be silently ignored", fn.Name)

				return true
			}

			if fn != nil && fn.Type.Results != nil && fn.Type.Results.List != nil && len(fn.Type.Results.List) > 0 {
				// if no returned errors, than check if there is any returned value that is ignored
				f.errorf(expr, 0.9, category("result-ignore"), "result of '%s' should not be silently ignored", fn.Name)

				return true
			}
		} else if asgn, ok := n.(*ast.AssignStmt); ok && asgn.Rhs != nil && len(asgn.Rhs) == 1 && asgn.Rhs[0] != nil {
			// process only assignments with single statement on the right. Here we analyze if any returned error
			// is ignored in statements like "a, b, c := fcall()"
			// at the moment assignments like "a, b := b, a" are not processed
			// TODO: do something with "a, b := f1(), f2()" and change if needed
			fn := extractFuncDecl(asgn.Rhs[0])
			errIndices := extractErrResultIndices(fn)

			for _, i := range errIndices {
				if i >= len(asgn.Lhs) {
					// This is situation when number of values on their right of assignment does not match number
					// of acceptors on left side. At the moment let's leave tis to compiler to report
					// TODO: maybe we should scream here? Or process this situation in another kind of check?

					return true
				}
				if ident, ok := asgn.Lhs[i].(*ast.Ident); ok && ident.Name == "_" {
					f.errorf(asgn, 0.8, category("result-ignore"), "function '%s' returns an error, generally it should not be intentionally ignored", fn.Name)

					return true
				}
			}
		}
		return true
	})
}

// try to extract declaration of fuction from given expression node
func extractFuncDecl(expr ast.Expr) *ast.FuncDecl {
	if cExpr, ok := expr.(*ast.CallExpr); ok && cExpr.Fun != nil {
		if ident, ok := cExpr.Fun.(*ast.Ident); ok && ident.Obj != nil && ident.Obj.Kind == ast.Fun && ident.Obj.Decl != nil {
			if fDecl, ok := ident.Obj.Decl.(*ast.FuncDecl); ok && fDecl.Type != nil && fDecl.Type.Results != nil && fDecl.Type.Results.List != nil {
				return fDecl
			}
		}
	}

	return nil
}

// from given function declaration extract indices of values in return result list which represent "error"
// Example: for definition of "func foo() (error, int, string, error)" it will return [0, 3]
func extractErrResultIndices(fn *ast.FuncDecl) (ind []int) {
	ind = make([]int, 0)
	if fn == nil || fn.Type.Results == nil || fn.Type.Results.List == nil {
		return
	}
	for i, field := range fn.Type.Results.List {
		if fieldIdent, ok := field.Type.(*ast.Ident); ok {
			if fieldIdent.Obj == nil && fieldIdent.Name == "error" {
				ind = append(ind, i)
			} else if fieldIdent.Obj != nil && fieldIdent.Obj.Kind == ast.Typ {
				// TODO: dig deeper to get what kind of object is it. Does it implement Error() ?
			}
		}
	}

	return
}

// lintNamedReturn examines function return values.
// It complains if any return value named.
func (f *file) lintNamedReturn() {
	f.walk(func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Type.Results == nil {
			return true
		}
		ret := fn.Type.Results.List

		i := 0
		for _, r := range ret {
			for _, varName := range r.Names {
				if f.render(varName) != "" {
					f.errorf(fn, 0.9, category("named-return"), "return value #%d(%q) should not be named", i, varName)
					i += 1
				}
			}
		}
		return true
	})
}

func receiverType(fn *ast.FuncDecl) string {
	switch e := fn.Recv.List[0].Type.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.StarExpr:
		return e.X.(*ast.Ident).Name
	}
	panic(fmt.Sprintf("unknown method receiver AST node type %T", fn.Recv.List[0].Type))
}

func (f *file) walk(fn func(ast.Node) bool) {
	ast.Walk(walker(fn), f.f)
}

func (f *file) render(x interface{}) string {
	var buf bytes.Buffer
	if err := printer.Fprint(&buf, f.fset, x); err != nil {
		panic(err)
	}
	return buf.String()
}

func (f *file) debugRender(x interface{}) string {
	var buf bytes.Buffer
	if err := ast.Fprint(&buf, f.fset, x, nil); err != nil {
		panic(err)
	}
	return buf.String()
}

// walker adapts a function to satisfy the ast.Visitor interface.
// The function return whether the walk should proceed into the node's children.
type walker func(ast.Node) bool

func (w walker) Visit(node ast.Node) ast.Visitor {
	if w(node) {
		return w
	}
	return nil
}

func isIdent(expr ast.Expr, ident string) bool {
	id, ok := expr.(*ast.Ident)
	return ok && id.Name == ident
}

// isBlank returns whether id is the blank identifier "_".
// If id == nil, the answer is false.
func isBlank(id *ast.Ident) bool { return id != nil && id.Name == "_" }

func isPkgDot(expr ast.Expr, pkg, name string) bool {
	sel, ok := expr.(*ast.SelectorExpr)
	return ok && isIdent(sel.X, pkg) && isIdent(sel.Sel, name)
}

func isZero(expr ast.Expr) bool {
	lit, ok := expr.(*ast.BasicLit)
	return ok && lit.Kind == token.INT && lit.Value == "0"
}

func isOne(expr ast.Expr) bool {
	lit, ok := expr.(*ast.BasicLit)
	return ok && lit.Kind == token.INT && lit.Value == "1"
}

var basicLitKindTypes = map[token.Token]string{
	token.FLOAT:  "float64",
	token.IMAG:   "complex128",
	token.CHAR:   "rune",
	token.STRING: "string",
}

// isUntypedConst reports whether expr is an untyped constant,
// and indicates what its default type is.
func isUntypedConst(expr ast.Expr) (defType string, ok bool) {
	if isIntLiteral(expr) {
		return "int", true
	}
	if bl, ok := expr.(*ast.BasicLit); ok {
		if dt, ok := basicLitKindTypes[bl.Kind]; ok {
			return dt, true
		}
	}
	return "", false
}

func isIntLiteral(expr ast.Expr) bool {
	// Either a BasicLit with Kind token.INT,
	// or some combination of a UnaryExpr with Op token.SUB (for "-<lit>")
	// or a ParenExpr (for "(<lit>)").
Loop:
	for {
		switch v := expr.(type) {
		case *ast.UnaryExpr:
			if v.Op == token.SUB {
				expr = v.X
				continue Loop
			}
		case *ast.ParenExpr:
			expr = v.X
			continue Loop
		case *ast.BasicLit:
			if v.Kind == token.INT {
				return true
			}
		}
		return false
	}
}

// srcLine returns the complete line at p, including the terminating newline.
func srcLine(src []byte, p token.Position) string {
	// Run to end of line in both directions if not at line start/end.
	lo, hi := p.Offset, p.Offset+1
	for lo > 0 && src[lo-1] != '\n' {
		lo--
	}
	for hi < len(src) && src[hi-1] != '\n' {
		hi++
	}
	return string(src[lo:hi])
}
