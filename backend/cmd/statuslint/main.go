package main

// 文件说明：statuslint 命令行入口，用于运行禁止直接改写状态字段的静态分析器。

import (
	"golang.org/x/tools/go/analysis/singlechecker"

	"qunxiang/backend/internal/infra/statuslint"
)

// main 启动 statuslint 静态分析器入口。
func main() {
	singlechecker.Main(statuslint.Analyzer)
}
