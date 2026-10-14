package wiki

import "github.com/primind/primind/internal/datasource/connector/feishu/core"

func txt(s string) *core.BlockText {
	return &core.BlockText{Elements: []core.TextElement{{TextRun: &core.TextRun{Content: s}}}}
}
