package pkg

import (
	"bytes"
	"go/ast"
	"go/format"
	"go/token"
	"slices"
	"strconv"
)

type syntaxKind int

const (
	syntaxUnknown syntaxKind = iota
	syntaxOther
	syntaxInt
	syntaxString
	syntaxSlice
	syntaxArray
	syntaxMap
	syntaxChan
	syntaxFunc
	syntaxPointerArray
)

type syntaxType struct {
	kind   syntaxKind
	length ast.Expr
	result *syntaxType
}

type syntaxScope struct {
	vars  map[string]syntaxType
	types map[string]ast.Expr
}

type syntaxVisitor struct {
	opts Options
	fset *token.FileSet

	sliceDeclarations   []*sliceDeclaration
	unknownDeclarations []*sliceDeclaration
	sliceAppends        []*sliceAppend
	loopVars            []ast.Expr
	level               int
	hasReturn           bool
	hasGoto             bool
	hasBranch           bool
	hasUnknown          bool
	diagnostics         []Diagnostic
	scopes              []syntaxScope
}

type createArrayResult struct {
	lenExpr ast.Expr
	ok      bool
	unknown bool
}

type countResult struct {
	expr    ast.Expr
	ok      bool
	unknown bool
}

type rangeCountResult struct {
	expr    ast.Expr
	ok      bool
	unknown bool
}

func CheckSyntax(fset *token.FileSet, files []*ast.File, opts Options) SyntaxResult {
	v := &syntaxVisitor{
		opts: opts,
		fset: fset,
		scopes: []syntaxScope{{
			vars:  map[string]syntaxType{},
			types: map[string]ast.Expr{},
		}},
	}
	for _, f := range files {
		v.collectPackageTypes(f)
	}
	for _, f := range files {
		v.collectPackageFuncs(f)
	}
	for _, f := range files {
		ast.Walk(v, f)
	}
	return SyntaxResult{
		Diagnostics: v.diagnostics,
		HasUnknown:  v.hasUnknown,
	}
}

func (v *syntaxVisitor) collectPackageTypes(file *ast.File) {
	for _, decl := range file.Decls {
		decl, ok := decl.(*ast.GenDecl)
		if !ok {
			continue
		}
		for _, spec := range decl.Specs {
			if ts, ok := spec.(*ast.TypeSpec); ok {
				v.scopes[0].types[ts.Name.Name] = ts.Type
			}
		}
	}
}

func (v *syntaxVisitor) collectPackageFuncs(file *ast.File) {
	for _, decl := range file.Decls {
		decl, ok := decl.(*ast.FuncDecl)
		if !ok || decl.Name == nil {
			continue
		}
		v.scopes[0].vars[decl.Name.Name] = v.funcType(decl.Type)
	}
}

func (v *syntaxVisitor) Visit(node ast.Node) ast.Visitor {
	switch s := node.(type) {
	case *ast.FuncDecl:
		if s.Body == nil {
			return nil
		}
		v.level = 0
		v.hasReturn = false
		v.hasGoto = false
		v.pushScope()
		v.addFieldTypes(s.Recv)
		v.addFieldTypes(s.Type.Params)
		ast.Walk(v, s.Body)
		v.popScope()
		return nil

	case *ast.FuncLit:
		if s.Body == nil {
			return nil
		}
		wasReturn := v.hasReturn
		wasGoto := v.hasGoto
		v.hasReturn = false
		v.pushScope()
		v.addFieldTypes(s.Type.Params)
		ast.Walk(v, s.Body)
		v.popScope()
		v.hasReturn = wasReturn
		v.hasGoto = wasGoto
		return nil

	case *ast.BlockStmt:
		declIdx := len(v.sliceDeclarations)
		unknownIdx := len(v.unknownDeclarations)
		appendIdx := len(v.sliceAppends)
		v.level++
		v.pushScope()
		for _, stmt := range s.List {
			ast.Walk(v, stmt)
		}
		v.popScope()
		v.level--

		buf := bytes.NewBuffer(nil)
		for i := declIdx; i < len(v.sliceDeclarations); i++ {
			sliceDecl := v.sliceDeclarations[i]
			if sliceDecl.exclude || v.hasGoto {
				continue
			}

			capExpr := sliceDecl.lenExpr
			for j := appendIdx; j < len(v.sliceAppends); j++ {
				if v.sliceAppends[j] != nil && v.sliceAppends[j].index == i {
					capExpr = addIntExpr(capExpr, v.sliceAppends[j].countExpr)
				}
			}
			if capExpr == sliceDecl.lenExpr {
				continue
			}
			if capVal, ok := intValue(capExpr); ok && capVal <= 0 {
				continue
			}

			buf.Reset()
			buf.WriteString("Consider preallocating ")
			buf.WriteString(sliceDecl.name)
			if capExpr != nil {
				undo := buf.Len()
				buf.WriteString(" with capacity ")
				if format.Node(buf, token.NewFileSet(), capExpr) != nil {
					buf.Truncate(undo)
				}
			}
			v.report(sliceDecl.pos, buf.String())
		}

		v.sliceDeclarations = v.sliceDeclarations[:declIdx]
		v.unknownDeclarations = v.unknownDeclarations[:unknownIdx]
		for i := appendIdx; i < len(v.sliceAppends); i++ {
			if v.sliceAppends[i] != nil {
				if v.sliceAppends[i].index >= declIdx {
					v.sliceAppends[i] = nil
				} else {
					appendIdx = i + 1
				}
			}
		}
		v.sliceAppends = v.sliceAppends[:appendIdx]
		return nil

	case *ast.TypeSpec:
		v.currentScope().types[s.Name.Name] = s.Type
		return nil

	case *ast.ValueSpec:
		v.handleValueSpec(s)

	case *ast.AssignStmt:
		v.handleAssignStmt(s)

	case *ast.CallExpr:
		v.handleCallExpr(s)

	case *ast.RangeStmt:
		return v.walkSyntaxRange(s)

	case *ast.ForStmt:
		return v.walkSyntaxFor(s)

	case *ast.SwitchStmt:
		return v.walkSyntaxSwitchSelect(s.Body)

	case *ast.TypeSwitchStmt:
		return v.walkSyntaxSwitchSelect(s.Body)

	case *ast.SelectStmt:
		return v.walkSyntaxSwitchSelect(s.Body)

	case *ast.ReturnStmt:
		if !v.opts.Simple {
			return nil
		}
		v.hasReturn = true
		for _, sliceApp := range v.sliceAppends {
			if sliceApp != nil {
				v.sliceDeclarations[sliceApp.index].hasReturn = true
			}
		}

	case *ast.BranchStmt:
		if !v.opts.Simple {
			return nil
		}
		if s.Label != nil {
			v.hasGoto = true
		} else {
			v.hasBranch = true
		}
	}
	return v
}

func (v *syntaxVisitor) report(pos token.Pos, message string) {
	p := v.fset.Position(pos)
	v.diagnostics = append(v.diagnostics, Diagnostic{
		Path:    p.Filename,
		Line:    p.Line,
		Column:  p.Column,
		Message: message,
	})
}

func (v *syntaxVisitor) handleValueSpec(s *ast.ValueSpec) {
	typeInfo, typeKnown := v.resolveType(s.Type)
	for i, name := range s.Names {
		var lenExpr ast.Expr
		var isSliceDecl bool
		var isUnknownDecl bool
		var varType syntaxType
		var varTypeKnown bool

		if i >= len(s.Values) {
			if typeKnown {
				varType = typeInfo
				varTypeKnown = true
				if isArrayOrSliceType(typeInfo) {
					isSliceDecl = true
					lenExpr = intExpr(0)
				}
			} else if s.Type != nil {
				isUnknownDecl = true
				varType = syntaxType{kind: syntaxUnknown}
				varTypeKnown = true
			}
		} else {
			value := s.Values[i]
			create := v.isCreateArray(value)
			if create.ok {
				isSliceDecl = true
				lenExpr = create.lenExpr
			} else if create.unknown {
				isUnknownDecl = true
			} else if id, ok := value.(*ast.Ident); ok && id.Name == "nil" {
				if typeKnown && isArrayOrSliceType(typeInfo) {
					isSliceDecl = true
					lenExpr = intExpr(0)
				} else if s.Type != nil && !typeKnown {
					isUnknownDecl = true
				}
			}

			if typeKnown && s.Type != nil {
				varType = typeInfo
				varTypeKnown = true
			} else if exprType, ok := v.exprType(value); ok {
				varType = exprType
				varTypeKnown = true
			} else if create.unknown || s.Type != nil {
				varType = syntaxType{kind: syntaxUnknown}
				varTypeKnown = true
			}
		}

		if isSliceDecl {
			v.sliceDeclarations = append(v.sliceDeclarations, &sliceDeclaration{name: name.Name, pos: s.Pos(), level: v.level, lenExpr: lenExpr})
		} else if isUnknownDecl {
			v.unknownDeclarations = append(v.unknownDeclarations, &sliceDeclaration{name: name.Name, pos: s.Pos(), level: v.level, lenExpr: intExpr(0)})
		}
		if varTypeKnown {
			v.currentScope().vars[name.Name] = varType
		}
	}
}

func (v *syntaxVisitor) handleAssignStmt(s *ast.AssignStmt) {
	if len(v.loopVars) > 0 {
		if len(s.Lhs) == len(s.Rhs) {
			for i, lhs := range s.Lhs {
				if hasAny(s.Rhs[i], v.loopVars) {
					v.loopVars = append(v.loopVars, lhs)
				}
			}
		} else if len(s.Rhs) == 1 && hasAny(s.Rhs[0], v.loopVars) {
			v.loopVars = append(v.loopVars, s.Lhs...)
		}
	}
	if len(s.Lhs) != len(s.Rhs) {
		return
	}
	for i, lhs := range s.Lhs {
		ident, ok := lhs.(*ast.Ident)
		if !ok {
			continue
		}
		create := v.isCreateArray(s.Rhs[i])
		switch {
		case create.ok:
			v.sliceDeclarations = append(v.sliceDeclarations, &sliceDeclaration{name: ident.Name, pos: s.Pos(), level: v.level, lenExpr: create.lenExpr})
		case create.unknown && s.Tok == token.DEFINE:
			v.unknownDeclarations = append(v.unknownDeclarations, &sliceDeclaration{name: ident.Name, pos: s.Pos(), level: v.level, lenExpr: intExpr(0)})
		default:
			declIdx := v.findSliceDeclaration(ident.Name)
			if declIdx >= 0 {
				sliceDecl := v.sliceDeclarations[declIdx]
				switch expr := s.Rhs[i].(type) {
				case *ast.Ident:
					if s.Tok == token.ASSIGN && expr.Name == "nil" {
						v.sliceDeclarations = append(v.sliceDeclarations, &sliceDeclaration{name: ident.Name, pos: s.Pos(), level: v.level, lenExpr: intExpr(0)})
						continue
					}
				case *ast.CallExpr:
					if len(expr.Args) >= 2 && !sliceDecl.hasReturn && sliceDecl.level == v.level {
						if funIdent, ok := expr.Fun.(*ast.Ident); ok && funIdent.Name == "append" {
							if rhsIdent, ok := expr.Args[0].(*ast.Ident); ok && ident.Name == rhsIdent.Name {
								sliceDecl.assigning = true
								continue
							}
						}
					}
				}
				sliceDecl.exclude = true
			}
		}

		if exprType, ok := v.exprType(s.Rhs[i]); ok {
			v.currentScope().vars[ident.Name] = exprType
		} else if create.unknown && s.Tok == token.DEFINE {
			v.currentScope().vars[ident.Name] = syntaxType{kind: syntaxUnknown}
		}
	}
}

func (v *syntaxVisitor) handleCallExpr(s *ast.CallExpr) {
	if funIdent, ok := s.Fun.(*ast.Ident); !ok || funIdent.Name != "append" || len(s.Args) < 2 {
		return
	}
	rhsIdent, ok := s.Args[0].(*ast.Ident)
	if !ok {
		return
	}
	declIdx := v.findSliceDeclaration(rhsIdent.Name)
	if declIdx < 0 {
		if v.findUnknownDeclaration(rhsIdent.Name) >= 0 {
			v.hasUnknown = true
		}
		return
	}
	sliceDecl := v.sliceDeclarations[declIdx]
	if sliceDecl.exclude {
		return
	}

	if sliceDecl.hasReturn || sliceDecl.level != v.level || sliceDecl.detached {
		sliceDecl.exclude = true
		return
	}

	count := v.appendCount(s)
	if count.unknown {
		v.hasUnknown = true
		sliceDecl.exclude = true
		return
	}
	countExpr := count.expr
	if countExpr != nil && (hasAny(countExpr, v.loopVars) || hasVarReference(countExpr, sliceDecl.name)) {
		sliceDecl.exclude = true
		return
	}

	if sliceDecl.assigning {
		sliceDecl.assigning = false
	} else {
		sliceDecl.detached = true
	}
	v.sliceAppends = append(v.sliceAppends, &sliceAppend{index: declIdx, countExpr: countExpr})
}

func (v *syntaxVisitor) walkSyntaxRange(stmt *ast.RangeStmt) ast.Visitor {
	if len(v.sliceDeclarations) == 0 && len(v.unknownDeclarations) == 0 {
		return v
	}
	if stmt.Body == nil {
		return nil
	}

	appendIdx := len(v.sliceAppends)
	hadBranch := v.hasBranch
	v.hasBranch = false
	v.level--
	varsIdx := len(v.loopVars)
	if stmt.Key != nil {
		v.loopVars = append(v.loopVars, stmt.Key)
	}
	if stmt.Value != nil {
		v.loopVars = append(v.loopVars, stmt.Value)
	}
	ast.Walk(v, stmt.Body)
	v.level++
	v.loopVars = v.loopVars[:varsIdx]

	exclude := !v.opts.IncludeRangeLoops || v.hasReturn || v.hasGoto || v.hasBranch
	var loopCountExpr ast.Expr
	if !exclude {
		count := v.rangeLoopCount(stmt)
		if count.unknown {
			if appendIdx < len(v.sliceAppends) {
				v.hasUnknown = true
			}
			exclude = true
		} else {
			loopCountExpr = count.expr
			exclude = !count.ok
		}
	}
	v.applyLoopCount(appendIdx, exclude, loopCountExpr)
	v.hasBranch = hadBranch
	return nil
}

func (v *syntaxVisitor) walkSyntaxFor(stmt *ast.ForStmt) ast.Visitor {
	if len(v.sliceDeclarations) == 0 && len(v.unknownDeclarations) == 0 {
		return v
	}
	if stmt.Body == nil {
		return nil
	}

	appendIdx := len(v.sliceAppends)
	hadBranch := v.hasBranch
	v.hasBranch = false
	v.level--
	varsIdx := len(v.loopVars)
	if assign, ok := stmt.Init.(*ast.AssignStmt); ok {
		v.loopVars = append(v.loopVars, assign.Lhs...)
	}
	ast.Walk(v, stmt.Body)
	v.level++
	v.loopVars = v.loopVars[:varsIdx]

	exclude := !v.opts.IncludeForLoops || v.hasReturn || v.hasGoto || v.hasBranch
	var loopCountExpr ast.Expr
	if !exclude {
		var ok bool
		loopCountExpr, ok = v.forLoopCount(stmt)
		exclude = !ok
	}
	v.applyLoopCount(appendIdx, exclude, loopCountExpr)
	v.hasBranch = hadBranch
	return nil
}

func (v *syntaxVisitor) walkSyntaxSwitchSelect(body *ast.BlockStmt) ast.Visitor {
	hadBranch := v.hasBranch
	v.hasBranch = false
	ast.Walk(v, body)
	v.hasBranch = hadBranch
	return nil
}

func (v *syntaxVisitor) applyLoopCount(appendIdx int, exclude bool, loopCountExpr ast.Expr) {
	if exclude {
		for i := appendIdx; i < len(v.sliceAppends); i++ {
			if v.sliceAppends[i] != nil {
				v.sliceDeclarations[v.sliceAppends[i].index].exclude = true
			}
		}
		return
	}
	for i, sliceDecl := range v.sliceDeclarations {
		if sliceDecl.exclude {
			continue
		}
		prev := -1
		for j := len(v.sliceAppends) - 1; j >= appendIdx; j-- {
			if v.sliceAppends[j] != nil && v.sliceAppends[j].index == i {
				if prev < 0 {
					if loopCountExpr == nil {
						v.sliceAppends[j].countExpr = nil
					} else if hasVarReference(loopCountExpr, sliceDecl.name) {
						sliceDecl.exclude = true
						break
					}
				} else {
					v.sliceAppends[j].countExpr = addIntExpr(v.sliceAppends[j].countExpr, v.sliceAppends[prev].countExpr)
					v.sliceAppends[prev] = nil
				}
				prev = j
			}
		}
		if prev >= 0 {
			v.sliceAppends[prev].countExpr = mulIntExpr(v.sliceAppends[prev].countExpr, loopCountExpr)
		}
	}
}

func (v *syntaxVisitor) isCreateArray(expr ast.Expr) createArrayResult {
	switch e := expr.(type) {
	case *ast.CompositeLit:
		t, ok := v.resolveType(e.Type)
		if !ok {
			return createArrayResult{unknown: e.Type != nil}
		}
		if isArrayOrSliceType(t) {
			return createArrayResult{lenExpr: intExpr(len(e.Elts)), ok: true}
		}
	case *ast.CallExpr:
		switch len(e.Args) {
		case 1:
			arg, ok := e.Args[0].(*ast.Ident)
			if !ok || arg.Name != "nil" {
				return createArrayResult{}
			}
			t, ok := v.resolveCallableType(e.Fun)
			if !ok {
				return createArrayResult{unknown: true}
			}
			if t.kind == syntaxSlice {
				return createArrayResult{lenExpr: intExpr(0), ok: true}
			}
		case 2:
			ident, ok := e.Fun.(*ast.Ident)
			if !ok || ident.Name != "make" {
				return createArrayResult{}
			}
			t, ok := v.resolveType(e.Args[0])
			if !ok {
				return createArrayResult{unknown: true}
			}
			if t.kind == syntaxSlice {
				return createArrayResult{lenExpr: e.Args[1], ok: true}
			}
		}
	}
	return createArrayResult{}
}

func (v *syntaxVisitor) rangeLoopCount(stmt *ast.RangeStmt) rangeCountResult {
	x := stmt.X
	if hasAny(x, v.loopVars) {
		return rangeCountResult{}
	}

	if call, ok := x.(*ast.CallExpr); ok {
		if len(call.Args) == 1 {
			if _, ok := call.Fun.(*ast.ArrayType); ok {
				x = call.Args[0]
			}
		} else if len(call.Args) >= 2 {
			if funIdent, ok := call.Fun.(*ast.Ident); ok && funIdent.Name == "append" {
				base := v.sliceLength(call.Args[0])
				added := v.appendCount(call)
				if base.unknown || added.unknown {
					return rangeCountResult{unknown: true}
				}
				return rangeCountResult{expr: addIntExpr(base.expr, added.expr), ok: true}
			}
		}
	}

	xType, ok := v.exprType(x)
	if !ok {
		return rangeCountResult{unknown: true}
	}

	switch xType.kind {
	case syntaxChan, syntaxFunc, syntaxOther:
		return rangeCountResult{}
	case syntaxArray:
		if _, ok := stmt.X.(*ast.CompositeLit); ok && xType.length != nil {
			return rangeCountResult{expr: xType.length, ok: true}
		}
	case syntaxSlice:
		if lit, ok := stmt.X.(*ast.CompositeLit); ok {
			return rangeCountResult{expr: intExpr(len(lit.Elts)), ok: true}
		}
	case syntaxMap:
		if lit, ok := x.(*ast.CompositeLit); ok {
			return rangeCountResult{expr: intExpr(len(lit.Elts)), ok: true}
		}
	case syntaxPointerArray:
		if unary, ok := x.(*ast.UnaryExpr); ok && unary.Op == token.AND && xType.length != nil {
			if _, ok := unary.X.(*ast.CompositeLit); ok {
				return rangeCountResult{expr: xType.length, ok: true}
			}
		}
	case syntaxString:
		if lit, ok := x.(*ast.BasicLit); ok && lit.Kind == token.STRING {
			if str, err := strconv.Unquote(lit.Value); err == nil {
				return rangeCountResult{expr: intExpr(len(str)), ok: true}
			}
		}
	case syntaxInt:
		if hasCall(x) {
			return rangeCountResult{ok: true}
		}
		return rangeCountResult{expr: x, ok: true}
	default:
		return rangeCountResult{unknown: true}
	}

	if hasCall(x) {
		return rangeCountResult{ok: true}
	}
	if slice, ok := x.(*ast.SliceExpr); ok {
		high := slice.High
		if high == nil {
			high = &ast.CallExpr{Fun: ast.NewIdent("len"), Args: []ast.Expr{slice.X}}
		}
		if slice.Low != nil {
			return rangeCountResult{expr: subIntExpr(high, slice.Low), ok: true}
		}
		return rangeCountResult{expr: high, ok: true}
	}
	return rangeCountResult{expr: &ast.CallExpr{Fun: ast.NewIdent("len"), Args: []ast.Expr{x}}, ok: true}
}

func (v *syntaxVisitor) appendCount(expr *ast.CallExpr) countResult {
	if expr.Ellipsis.IsValid() {
		return v.sliceLength(expr.Args[1])
	}
	return countResult{expr: intExpr(len(expr.Args) - 1), ok: true}
}

func (v *syntaxVisitor) sliceLength(expr ast.Expr) countResult {
	if call, ok := expr.(*ast.CallExpr); ok {
		if len(call.Args) == 1 {
			if _, ok := call.Fun.(*ast.ArrayType); ok {
				expr = call.Args[0]
			}
		} else if len(call.Args) >= 2 {
			if funIdent, ok := call.Fun.(*ast.Ident); ok && funIdent.Name == "append" {
				base := v.sliceLength(call.Args[0])
				added := v.appendCount(call)
				if base.unknown || added.unknown {
					return countResult{unknown: true}
				}
				return countResult{expr: addIntExpr(base.expr, added.expr), ok: true}
			}
		}
	}

	xType, ok := v.exprType(expr)
	if !ok {
		return countResult{unknown: true}
	}
	switch xType.kind {
	case syntaxArray, syntaxSlice:
		if lit, ok := expr.(*ast.CompositeLit); ok {
			return countResult{expr: intExpr(len(lit.Elts)), ok: true}
		}
	case syntaxString:
		if lit, ok := expr.(*ast.BasicLit); ok && lit.Kind == token.STRING {
			if str, err := strconv.Unquote(lit.Value); err == nil {
				return countResult{expr: intExpr(len(str)), ok: true}
			}
		}
	default:
		return countResult{unknown: xType.kind == syntaxUnknown}
	}

	if hasCall(expr) {
		return countResult{ok: true}
	}
	if slice, ok := expr.(*ast.SliceExpr); ok {
		high := slice.High
		if high == nil {
			high = &ast.CallExpr{Fun: ast.NewIdent("len"), Args: []ast.Expr{slice.X}}
		}
		if slice.Low != nil {
			return countResult{expr: subIntExpr(high, slice.Low), ok: true}
		}
		return countResult{expr: high, ok: true}
	}
	return countResult{expr: &ast.CallExpr{Fun: ast.NewIdent("len"), Args: []ast.Expr{expr}}, ok: true}
}

func (v *syntaxVisitor) forLoopCount(stmt *ast.ForStmt) (ast.Expr, bool) {
	return (&returnsVisitor{loopVars: v.loopVars}).forLoopCount(stmt)
}

func (v *syntaxVisitor) pushScope() {
	v.scopes = append(v.scopes, syntaxScope{vars: map[string]syntaxType{}, types: map[string]ast.Expr{}})
}

func (v *syntaxVisitor) popScope() {
	v.scopes = v.scopes[:len(v.scopes)-1]
}

func (v *syntaxVisitor) currentScope() *syntaxScope {
	return &v.scopes[len(v.scopes)-1]
}

func (v *syntaxVisitor) addFieldTypes(fields *ast.FieldList) {
	if fields == nil {
		return
	}
	for _, field := range fields.List {
		t, ok := v.resolveType(field.Type)
		if !ok {
			t = syntaxType{kind: syntaxUnknown}
		}
		for _, name := range field.Names {
			v.currentScope().vars[name.Name] = t
		}
	}
}

func (v *syntaxVisitor) exprType(expr ast.Expr) (syntaxType, bool) {
	switch e := expr.(type) {
	case nil:
		return syntaxType{}, false
	case *ast.ParenExpr:
		return v.exprType(e.X)
	case *ast.Ident:
		if e.Name == "nil" {
			return syntaxType{kind: syntaxOther}, true
		}
		return v.lookupVar(e.Name)
	case *ast.BasicLit:
		switch e.Kind {
		case token.INT:
			return syntaxType{kind: syntaxInt}, true
		case token.STRING:
			return syntaxType{kind: syntaxString}, true
		default:
			return syntaxType{kind: syntaxOther}, true
		}
	case *ast.CompositeLit:
		return v.resolveType(e.Type)
	case *ast.FuncLit:
		return v.funcType(e.Type), true
	case *ast.CallExpr:
		return v.callType(e)
	case *ast.UnaryExpr:
		if e.Op == token.AND {
			if t, ok := v.exprType(e.X); ok && t.kind == syntaxArray {
				return syntaxType{kind: syntaxPointerArray, length: t.length}, true
			}
		}
		if e.Op == token.ADD || e.Op == token.SUB {
			if t, ok := v.exprType(e.X); ok && t.kind == syntaxInt {
				return t, true
			}
		}
	case *ast.BinaryExpr:
		x, xOK := v.exprType(e.X)
		y, yOK := v.exprType(e.Y)
		if xOK && yOK && x.kind == syntaxInt && y.kind == syntaxInt {
			return syntaxType{kind: syntaxInt}, true
		}
		if xOK && yOK && x.kind == syntaxString && y.kind == syntaxString && e.Op == token.ADD {
			return syntaxType{kind: syntaxString}, true
		}
	case *ast.SliceExpr:
		t, ok := v.exprType(e.X)
		if !ok {
			return syntaxType{}, false
		}
		switch t.kind {
		case syntaxArray, syntaxPointerArray, syntaxSlice:
			return syntaxType{kind: syntaxSlice}, true
		case syntaxString:
			return syntaxType{kind: syntaxString}, true
		}
	}
	return syntaxType{}, false
}

func (v *syntaxVisitor) callType(call *ast.CallExpr) (syntaxType, bool) {
	if len(call.Args) == 1 {
		if t, ok := v.resolveCallableType(call.Fun); ok {
			return t, true
		}
	}
	if ident, ok := call.Fun.(*ast.Ident); ok {
		switch ident.Name {
		case "make":
			if len(call.Args) == 0 {
				return syntaxType{}, false
			}
			return v.resolveType(call.Args[0])
		case "append":
			if len(call.Args) == 0 {
				return syntaxType{}, false
			}
			return v.exprType(call.Args[0])
		case "len", "cap":
			return syntaxType{kind: syntaxInt}, true
		case "min", "max":
			for _, arg := range call.Args {
				if t, ok := v.exprType(arg); !ok || t.kind != syntaxInt {
					return syntaxType{}, false
				}
			}
			return syntaxType{kind: syntaxInt}, true
		}
		if fn, ok := v.lookupVar(ident.Name); ok && fn.kind == syntaxFunc {
			if fn.result == nil {
				return syntaxType{kind: syntaxOther}, true
			}
			return *fn.result, true
		}
	}
	if lit, ok := call.Fun.(*ast.FuncLit); ok {
		fn := v.funcType(lit.Type)
		if fn.result != nil {
			return *fn.result, true
		}
		return syntaxType{kind: syntaxOther}, true
	}
	return syntaxType{}, false
}

func (v *syntaxVisitor) resolveCallableType(expr ast.Expr) (syntaxType, bool) {
	switch e := expr.(type) {
	case *ast.Ident:
		if t, ok := builtinType(e.Name); ok {
			return t, true
		}
		if typeExpr, ok := v.lookupTypeExpr(e.Name); ok {
			return v.resolveTypeSeen(typeExpr, map[string]bool{e.Name: true})
		}
	case *ast.ArrayType, *ast.MapType, *ast.ChanType, *ast.FuncType, *ast.StarExpr:
		return v.resolveType(e)
	}
	return syntaxType{}, false
}

func (v *syntaxVisitor) resolveType(expr ast.Expr) (syntaxType, bool) {
	return v.resolveTypeSeen(expr, map[string]bool{})
}

func (v *syntaxVisitor) resolveTypeSeen(expr ast.Expr, seen map[string]bool) (syntaxType, bool) {
	switch e := expr.(type) {
	case nil:
		return syntaxType{}, false
	case *ast.ParenExpr:
		return v.resolveTypeSeen(e.X, seen)
	case *ast.Ident:
		if t, ok := builtinType(e.Name); ok {
			return t, true
		}
		if seen[e.Name] {
			return syntaxType{}, false
		}
		typeExpr, ok := v.lookupTypeExpr(e.Name)
		if !ok {
			return syntaxType{}, false
		}
		seen[e.Name] = true
		return v.resolveTypeSeen(typeExpr, seen)
	case *ast.ArrayType:
		if e.Len == nil {
			return syntaxType{kind: syntaxSlice}, true
		}
		if _, ok := e.Len.(*ast.Ellipsis); ok {
			return syntaxType{kind: syntaxArray}, true
		}
		return syntaxType{kind: syntaxArray, length: e.Len}, true
	case *ast.MapType:
		return syntaxType{kind: syntaxMap}, true
	case *ast.ChanType:
		return syntaxType{kind: syntaxChan}, true
	case *ast.FuncType:
		return v.funcType(e), true
	case *ast.StarExpr:
		elem, ok := v.resolveTypeSeen(e.X, seen)
		if !ok {
			return syntaxType{}, false
		}
		if elem.kind == syntaxArray {
			return syntaxType{kind: syntaxPointerArray, length: elem.length}, true
		}
		return syntaxType{kind: syntaxOther}, true
	}
	return syntaxType{}, false
}

func (v *syntaxVisitor) funcType(t *ast.FuncType) syntaxType {
	fn := syntaxType{kind: syntaxFunc}
	if t == nil || t.Results == nil || len(t.Results.List) != 1 {
		return fn
	}
	result := t.Results.List[0]
	if len(result.Names) > 1 {
		return fn
	}
	if rt, ok := v.resolveType(result.Type); ok {
		fn.result = &rt
	}
	return fn
}

func (v *syntaxVisitor) lookupVar(name string) (syntaxType, bool) {
	for _, scope := range slices.Backward(v.scopes) {
		if t, ok := scope.vars[name]; ok {
			return t, true
		}
	}
	return syntaxType{}, false
}

func (v *syntaxVisitor) lookupTypeExpr(name string) (ast.Expr, bool) {
	for _, scope := range slices.Backward(v.scopes) {
		if expr, ok := scope.types[name]; ok {
			return expr, true
		}
	}
	return nil, false
}

func (v *syntaxVisitor) findSliceDeclaration(name string) int {
	for i, sliceDecl := range slices.Backward(v.sliceDeclarations) {
		if sliceDecl.name == name {
			return i
		}
	}
	return -1
}

func (v *syntaxVisitor) findUnknownDeclaration(name string) int {
	for i, sliceDecl := range slices.Backward(v.unknownDeclarations) {
		if sliceDecl.name == name {
			return i
		}
	}
	return -1
}

func builtinType(name string) (syntaxType, bool) {
	switch name {
	case "byte", "rune", "int", "int8", "int16", "int32", "int64",
		"uint", "uint8", "uint16", "uint32", "uint64", "uintptr":
		return syntaxType{kind: syntaxInt}, true
	case "string":
		return syntaxType{kind: syntaxString}, true
	case "bool", "error", "any",
		"float32", "float64", "complex64", "complex128":
		return syntaxType{kind: syntaxOther}, true
	}
	return syntaxType{}, false
}

func isArrayOrSliceType(t syntaxType) bool {
	return t.kind == syntaxArray || t.kind == syntaxSlice
}
