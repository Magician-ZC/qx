package session

// 文件说明：跨玩家结算路径「不裸 units.Save(peer)」的源码级 lint 守卫（修复 major-1 的最低限度补强）。
//
// 设计宪法红线：B 永远只能写一条 cross_event，改不了 A 的 units。跨玩家结算路径绝大多数根本不写他人 units——
// 只写本侧 relations + append cross_event。本测试在 **AST 层** 扫描跨玩家结算源文件，断言它们**不出现裸的
// `service.units.Save(...)` / `service.units.SaveOptimistic(...)` 调用**：任何「确需整记录写单位」必须改道经
// `saveOwnSideUnit`（带 own-side 归属断言 + 审计 + storage 层 SaveOwnedBy 二次硬守）。这样即便未来有人在跨玩家
// 路径误加裸 units.Save，CI 立即红，避免红线被悄悄绕过（把「装好的断言」真正接成「绕不过去的纪律」）。
//
// 注意：本守卫扫的是「跨玩家结算」一组文件（consent/seven/auto_match/contest/relation/cross_write_guard/social/cross_link/
// threat_propagation/worldize）。这些文件里出现 units.Save 即可疑——它们的设计契约是「只写本侧 relations + cross_event」。
// 非跨玩家文件（service.go/executor_loop.go 等本侧主循环）合法使用 units.Save 写**本侧**单位，不在本守卫范围。

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// crossPlayerSettlementFiles 是「跨玩家结算」源文件名单：设计契约为「只写本侧 relations + append cross_event」，
// 绝不整记录写他人 units。新增跨玩家结算文件时应登记进来（守卫扩面）。
var crossPlayerSettlementFiles = []string{
	"consent_gate.go",
	"seven_interactions.go",
	"auto_match.go",
	"contest.go",
	"cross_link.go",
	"social_match.go",
	"social_scan.go",
	"social_scan_cross.go",
	"threat_propagation.go",
	"worldize_inbound.go",
	"cross_write_guard.go",
}

// TestCrossPlayerPaths_NoBareUnitsSave 断言跨玩家结算源文件里不出现裸 service.units.Save / .SaveOptimistic 调用。
// 在 AST 层判定「选择器表达式 X.units.Save(...) 形式的函数调用」，注释/字符串里的 "units.Save" 文本不计（go/parser 丢弃注释）。
// 若需在某跨玩家路径整记录写本侧单位，请改用 service.saveOwnSideUnit（带 own-side 断言 + 审计 + SaveOwnedBy 硬守）。
func TestCrossPlayerPaths_NoBareUnitsSave(t *testing.T) {
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for _, name := range crossPlayerSettlementFiles {
		path := filepath.Join(dir, name)
		if _, statErr := os.Stat(path); statErr != nil {
			// 文件被重命名/删除：跳过（不因名单滞后误红；新增文件靠上面登记扩面）。
			continue
		}
		fset := token.NewFileSet()
		file, perr := parser.ParseFile(fset, path, nil, 0) // 0 = 不保留注释：注释里的 "units.Save" 文本天然不进 AST
		if perr != nil {
			t.Fatalf("parse %s: %v", name, perr)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			// 形如  <expr>.Save(...) / <expr>.SaveOptimistic(...)，且其 <expr> 末段选择子是 "units"。
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if sel.Sel.Name != "Save" && sel.Sel.Name != "SaveOptimistic" {
				return true
			}
			inner, ok := sel.X.(*ast.SelectorExpr) // 期望 X 形如 service.units
			if !ok {
				return true
			}
			if inner.Sel.Name == "units" {
				pos := fset.Position(call.Pos())
				t.Errorf("跨玩家结算路径出现裸 units.%s（违反「只改本侧」红线）：%s:%d —— 请改道经 saveOwnSideUnit（own-side 断言 + 审计）",
					sel.Sel.Name, name, pos.Line)
			}
			return true
		})
	}
}

// TestCrossPlayerLintScanner_Sanity 自检：扫描器能在一段构造源上正确识别 service.units.Save(...) 调用（避免守卫静默失效）。
func TestCrossPlayerLintScanner_Sanity(t *testing.T) {
	src := `package p
func f(service *S) { service.units.Save(ctx, rec); service.saveOwnSideUnit(ctx, "s", rec, "r") }`
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "sanity.go", src, 0)
	if err != nil {
		t.Fatalf("parse sanity: %v", err)
	}
	hits := 0
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || (sel.Sel.Name != "Save" && sel.Sel.Name != "SaveOptimistic") {
			return true
		}
		if inner, ok := sel.X.(*ast.SelectorExpr); ok && inner.Sel.Name == "units" {
			hits++
		}
		return true
	})
	if hits != 1 {
		t.Fatalf("扫描器自检应恰好识别 1 处 units.Save，得 %d（守卫可能已静默失效）", hits)
	}
	// saveOwnSideUnit 不是 units.Save，不应被计入（确保守卫不误伤合规改道）。
	if strings.Count(src, "saveOwnSideUnit") != 1 {
		t.Fatalf("自检源应含 1 处 saveOwnSideUnit")
	}
}
