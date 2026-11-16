package server

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// TestConsoleSmoke runs scripts/console-smoke.mjs: it actually executes
// web/js/*.js with a minimal DOM stub (importing triggers all top-level wiring) and
// feeds fixed data to the four render functions.
//
// Why it exists: the console is pure frontend, and Go tests can only assert "the
// string exists". When splitting modules, the most likely breakage is a reference
// break - files all present, syntax valid, `node --check` passes too (it parses as
// CJS, so it lets an ESM top-level return through), and only clicking a button blows
// up. During this round of splitting it caught three such problems:
// - a leftover `lang =...` in app.js (a module-private variable assigned directly);
//   - the same symbol imported twice;
//   - a top-level `return` left behind after removing the IIFE wrapper from the drawer
//     section.
//
// Skipped when node is not installed: CI (ubuntu-latest ships Node) really runs it;
// install locally as needed.
func TestConsoleSmoke(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("未安装 node，跳过控制台冒烟测试")
	}
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("解析仓库根目录: %v", err)
	}
	cmd := exec.Command(node, filepath.Join("scripts", "console-smoke.mjs"))
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("控制台冒烟失败：%v\n%s", err, out)
	}
	t.Logf("控制台冒烟通过：\n%s", out)
}
