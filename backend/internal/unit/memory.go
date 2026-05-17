package unit

// 文件说明：单位轻量记忆工具，维护 highlights 追加去重与最近记忆切片读取。

import "strings"

const maxMemoryHighlights = 12

// Remember 向单位记忆高亮列表追加一条摘要，并按上限截断。
func Remember(record *Record, summary string) {
	if record == nil {
		return
	}

	summary = strings.TrimSpace(summary)
	if summary == "" {
		return
	}

	highlights := append([]string{}, record.Memory.Highlights...)
	if len(highlights) > 0 && highlights[len(highlights)-1] == summary {
		return
	}

	highlights = append(highlights, summary)
	if len(highlights) > maxMemoryHighlights {
		highlights = highlights[len(highlights)-maxMemoryHighlights:]
	}
	record.Memory.Highlights = highlights
}

// RecentHighlights 返回单位最近 N 条记忆高亮副本。
func RecentHighlights(record Record, limit int) []string {
	if limit <= 0 || len(record.Memory.Highlights) == 0 {
		return nil
	}
	if len(record.Memory.Highlights) <= limit {
		return append([]string{}, record.Memory.Highlights...)
	}
	return append([]string{}, record.Memory.Highlights[len(record.Memory.Highlights)-limit:]...)
}
