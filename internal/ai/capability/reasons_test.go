package capability

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The reasons a model may be told (reasons.txt, StatedReason) are exactly the string literals that
// stand at a reason position of a rejection in the product's source. This test finds them: it
// parses every non-test Go file of the roots below, starts from kernel.Reject and kernel.RejectAs,
// follows every function that hands one of its parameters on as the reason (fail(code, path,
// reason), a context's Reject, a local closure), and collects the literal texts given there. A
// reason built at run time is a "dynamic site": it is counted, listed on request, and never in the
// list, so it never reaches a model.
var reasonRoots = []string{"internal/kernel", "internal/foundation", "internal/ai", "internal/contexts"}

const reasonModule = "go.idp.xyz/idp-brant"

type reasonFunc struct {
	key    string // "<dir>.<name>"
	params []string
	body   *ast.BlockStmt
	file   *reasonFile
}

type reasonFile struct {
	dir     string
	path    string
	ast     *ast.File
	imports map[string]string // import name → directory in the module, "" for a package outside it
}

type reasonScan struct {
	fset   *token.FileSet
	files  []*reasonFile
	funcs  []*reasonFunc
	consts map[string]string       // "<dir>.<name>" → the text of a package-level string constant
	ctors  map[string]map[int]bool // "<dir>.<name>" → the argument positions that become a reason
	flows  map[token.Pos]bool      // arguments that are a parameter handed on
}

func scanReasons(t *testing.T) *reasonScan {
	t.Helper()
	root := filepath.Join("..", "..", "..")
	s := &reasonScan{fset: token.NewFileSet(), consts: map[string]string{}, flows: map[token.Pos]bool{},
		ctors: map[string]map[int]bool{"internal/kernel.Reject": {2: true}, "internal/kernel.RejectAs": {3: true}}}
	for _, r := range reasonRoots {
		err := filepath.WalkDir(filepath.Join(root, filepath.FromSlash(r)), func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			f, err := parser.ParseFile(s.fset, path, nil, parser.SkipObjectResolution)
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(root, filepath.Dir(path))
			rf := &reasonFile{dir: filepath.ToSlash(rel), path: filepath.ToSlash(path), ast: f, imports: map[string]string{}}
			for _, im := range f.Imports {
				p, _ := strconv.Unquote(im.Path.Value)
				name := p[strings.LastIndex(p, "/")+1:]
				if im.Name != nil {
					name = im.Name.Name
				}
				rf.imports[name] = ""
				if dir, ok := strings.CutPrefix(p, reasonModule+"/"); ok {
					rf.imports[name] = dir
				}
			}
			s.files = append(s.files, rf)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range s.files {
		s.declare(f)
	}
	// A function that hands a parameter on as a reason is itself a place where reasons are given.
	for changed := true; changed; {
		changed = false
		for _, fn := range s.funcs {
			ast.Inspect(fn.body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				for i := range s.ctors[s.callee(call, fn.file)] {
					if i >= len(call.Args) {
						continue
					}
					id, ok := call.Args[i].(*ast.Ident)
					if !ok {
						continue
					}
					if j := slices.Index(fn.params, id.Name); j >= 0 {
						s.flows[id.Pos()] = true
						if s.ctors[fn.key] == nil {
							s.ctors[fn.key] = map[int]bool{}
						}
						if !s.ctors[fn.key][j] {
							s.ctors[fn.key][j], changed = true, true
						}
					}
				}
				return true
			})
		}
	}
	return s
}

// declare records the functions of a file (declarations, and function literals bound to a name)
// and its package-level string constants.
func (s *reasonScan) declare(f *reasonFile) {
	add := func(name string, typ *ast.FuncType, body *ast.BlockStmt) {
		if body == nil {
			return
		}
		fn := &reasonFunc{key: f.dir + "." + name, body: body, file: f}
		for _, field := range typ.Params.List {
			if len(field.Names) == 0 {
				fn.params = append(fn.params, "_")
			}
			for _, n := range field.Names {
				fn.params = append(fn.params, n.Name)
			}
		}
		s.funcs = append(s.funcs, fn)
	}
	for _, d := range f.ast.Decls {
		switch d := d.(type) {
		case *ast.FuncDecl:
			add(d.Name.Name, d.Type, d.Body)
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, name := range vs.Names {
					if i < len(vs.Values) {
						if text, ok := s.literal(vs.Values[i], f); ok {
							s.consts[f.dir+"."+name.Name] = text
						}
					}
				}
			}
		}
	}
	ast.Inspect(f.ast, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.AssignStmt:
			for i, rhs := range n.Rhs {
				if lit, ok := rhs.(*ast.FuncLit); ok && i < len(n.Lhs) {
					if id, ok := n.Lhs[i].(*ast.Ident); ok {
						add(id.Name, lit.Type, lit.Body)
					}
				}
			}
		case *ast.ValueSpec:
			for i, v := range n.Values {
				if lit, ok := v.(*ast.FuncLit); ok && i < len(n.Names) {
					add(n.Names[i].Name, lit.Type, lit.Body)
				}
			}
		}
		return true
	})
}

// callee names the function a call reaches: one of the file's package, or one of an imported
// package of the module. A method called on a value is looked for in the file's package.
func (s *reasonScan) callee(call *ast.CallExpr, f *reasonFile) string {
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		return f.dir + "." + fun.Name
	case *ast.SelectorExpr:
		if x, ok := fun.X.(*ast.Ident); ok {
			if dir, imported := f.imports[x.Name]; imported {
				if dir == "" {
					return ""
				}
				return dir + "." + fun.Sel.Name
			}
		}
		return f.dir + "." + fun.Sel.Name
	}
	return ""
}

// literal returns the text of an expression that is a string literal, a package-level string
// constant or a concatenation of such.
func (s *reasonScan) literal(e ast.Expr, f *reasonFile) (string, bool) {
	switch e := e.(type) {
	case *ast.BasicLit:
		if e.Kind != token.STRING {
			return "", false
		}
		text, err := strconv.Unquote(e.Value)
		return text, err == nil
	case *ast.ParenExpr:
		return s.literal(e.X, f)
	case *ast.Ident:
		text, ok := s.consts[f.dir+"."+e.Name]
		return text, ok
	case *ast.BinaryExpr:
		if e.Op != token.ADD {
			return "", false
		}
		a, okA := s.literal(e.X, f)
		b, okB := s.literal(e.Y, f)
		return a + b, okA && okB
	}
	return "", false
}

// collect returns the literal reasons of the source, and the places where a reason is built at
// run time.
func (s *reasonScan) collect() (literals []string, dynamic []string) {
	seen := map[string]bool{}
	note := func(e ast.Expr, f *reasonFile) {
		if text, ok := s.literal(e, f); ok {
			if text != "" && !seen[text] {
				seen[text] = true
				literals = append(literals, text)
			}
			return
		}
		if s.flows[e.Pos()] || f.path == "../../../internal/kernel/rejection.go" {
			return // a parameter handed on: judged where the function is called
		}
		p := s.fset.Position(e.Pos())
		dynamic = append(dynamic, fmt.Sprintf("%s:%d", strings.TrimPrefix(filepath.ToSlash(p.Filename), "../../../"), p.Line))
	}
	fieldErrors := func(lit *ast.CompositeLit, f *reasonFile) {
		for _, el := range lit.Elts {
			if kv, ok := el.(*ast.KeyValueExpr); ok {
				if k, ok := kv.Key.(*ast.Ident); ok && k.Name == "Reason" {
					note(kv.Value, f)
				}
			}
		}
	}
	isFieldError := func(e ast.Expr) bool {
		switch e := e.(type) {
		case *ast.Ident:
			return e.Name == "FieldError"
		case *ast.SelectorExpr:
			return e.Sel.Name == "FieldError"
		}
		return false
	}
	for _, f := range s.files {
		ast.Inspect(f.ast, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.CallExpr:
				for i := range s.ctors[s.callee(n, f)] {
					if i < len(n.Args) {
						note(n.Args[i], f)
					}
				}
			case *ast.CompositeLit:
				switch typ := n.Type.(type) {
				case *ast.ArrayType:
					if isFieldError(typ.Elt) {
						for _, el := range n.Elts {
							if inner, ok := el.(*ast.CompositeLit); ok {
								fieldErrors(inner, f)
							}
						}
					}
				default:
					if n.Type != nil && isFieldError(n.Type) {
						fieldErrors(n, f)
					}
				}
			}
			return true
		})
	}
	sort.Strings(literals)
	sort.Strings(dynamic)
	return literals, slices.Compact(dynamic)
}

func TestStatedReasonsAreTheSourcesLiterals(t *testing.T) {
	s := scanReasons(t)
	literals, dynamic := s.collect()
	var b strings.Builder
	for _, text := range literals {
		b.WriteString(strconv.Quote(text) + "\n")
	}
	if os.Getenv("BRANT_UPDATE_REASONS") == "1" {
		if err := os.WriteFile("reasons.txt", []byte(b.String()), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("reasons.txt written: %d texts", len(literals))
	} else if b.String() != reasonList {
		t.Fatalf("reasons.txt is not the source's literal reasons (%d texts found); regenerate it with BRANT_UPDATE_REASONS=1 "+
			"go test ./internal/ai/capability -run TestStatedReasonsAreTheSourcesLiterals", len(literals))
	}
	t.Logf("%d literal reasons; %d places build a reason at run time (never passed to a model); %d functions hand a reason on",
		len(literals), len(dynamic), len(s.ctors))
	if os.Getenv("BRANT_SHOW_REASONS") == "1" {
		t.Logf("run-time reasons at:\n%s", strings.Join(dynamic, "\n"))
	}
	if len(literals) < 200 {
		t.Fatalf("only %d literal reasons found: the scan lost its way", len(literals))
	}
	// Reasons the Q01 tools meet are in the list; reasons built at run time are not.
	for _, text := range []string{"Name the place verified", "The reference is not of the kind its field takes", "Source is not accessible",
		"Human confirmation must verify every line", "set by the platform", "the gateway serves AI actors"} {
		if StatedReason("SOME_CODE", text) != text {
			t.Errorf("%q is a literal reason of the source and is not passed", text)
		}
	}
	for code, text := range map[string]string{"DRAFT_NOT_READY": "Pending: missing", "SOURCE_AMBIGUOUS": "4 places: 5:37, 6:37, 7:37, 8:37",
		"SCHEMA_INVALID": "SCHEMA_INVALID", "NOT_FOUND": "", "INVALID_STATE": "The action is committed"} {
		if got := StatedReason(code, text); got != "" {
			t.Errorf("%q is passed as a reason of %s", got, code)
		}
	}
}
