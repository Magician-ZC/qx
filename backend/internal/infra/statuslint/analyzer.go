package statuslint

// 文件说明：statuslint 静态规则实现，检测并阻止白名单外代码直接改写受保护状态字段。

import (
	"go/ast"
	"go/types"
	"path/filepath"
	"strings"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/passes/inspect"
	"golang.org/x/tools/go/ast/inspector"
)

var protectedFields = map[string]struct{}{
	"HP":             {},
	"Hunger":         {},
	"Morale":         {},
	"Loyalty":        {},
	"LivesRemaining": {},
	"Mood":           {},
}

var Analyzer = &analysis.Analyzer{
	Name:     "statuslint",
	Doc:      "forbid direct mutation of protected unit status fields outside approved files",
	Requires: []*analysis.Analyzer{inspect.Analyzer},
	Run:      run,
}

// run 扫描赋值与自增自减语句，拦截对受保护状态字段的直接写操作。
func run(pass *analysis.Pass) (any, error) {
	inspectResult := pass.ResultOf[inspect.Analyzer].(*inspector.Inspector)
	inspectResult.Preorder(
		[]ast.Node{(*ast.AssignStmt)(nil), (*ast.IncDecStmt)(nil)},
		func(node ast.Node) {
			filename := pass.Fset.Position(node.Pos()).Filename
			if isAllowedFile(filename) {
				return
			}

			switch statement := node.(type) {
			case *ast.AssignStmt:
				for _, lhs := range statement.Lhs {
					if isProtectedStatusSelector(pass, lhs) {
						pass.Reportf(lhs.Pos(), "direct mutation of protected unit status is forbidden; use StatusMutator")
					}
				}
			case *ast.IncDecStmt:
				if isProtectedStatusSelector(pass, statement.X) {
					pass.Reportf(statement.X.Pos(), "direct mutation of protected unit status is forbidden; use StatusMutator")
				}
			}
		},
	)

	return nil, nil
}

// isProtectedStatusSelector 判断表达式是否指向 unit.Status 的受保护字段。
func isProtectedStatusSelector(pass *analysis.Pass, expression ast.Expr) bool {
	selector, ok := expression.(*ast.SelectorExpr)
	if !ok {
		return false
	}

	if _, ok := protectedFields[selector.Sel.Name]; !ok {
		return false
	}

	ownerType := pass.TypesInfo.TypeOf(selector.X)
	if ownerType == nil {
		return false
	}

	named, ok := dereference(ownerType).(*types.Named)
	if !ok || named.Obj() == nil {
		return false
	}

	pkg := named.Obj().Pkg()
	if pkg == nil {
		return false
	}

	return pkg.Path() == "qunxiang/backend/internal/unit" && named.Obj().Name() == "Status"
}

// dereference 对指针类型解引用，返回其底层元素类型。
func dereference(value types.Type) types.Type {
	if pointer, ok := value.(*types.Pointer); ok {
		return pointer.Elem()
	}
	return value
}

// isAllowedFile 判断文件是否属于允许直接改状态字段的白名单。
func isAllowedFile(filename string) bool {
	normalized := filepath.ToSlash(filename)
	if strings.HasSuffix(normalized, "_test.go") {
		return true
	}

	allowedFragments := []string{
		"/internal/engine/status/",
		"/internal/unit/repository.go",
		"/internal/unit/lives.go",
	}

	for _, fragment := range allowedFragments {
		if strings.Contains(normalized, fragment) {
			return true
		}
	}

	return false
}
