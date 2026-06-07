package session

// 文件说明：牵挂等级随真实共创（处理待决策）上升的集成测试（对真实 SQLite）。

import (
	"context"
	"testing"

	"qunxiang/backend/internal/unit"
)

func TestComputeAttachmentRisesWithCoCreation(t *testing.T) {
	_, repo, service := newThreatTestService(t)
	ctx := context.Background()
	rec := unit.BootstrapRecord(9, "s1", "player", "她")
	if err := repo.Save(ctx, rec); err != nil {
		t.Fatalf("存角色失败: %v", err)
	}

	// 固定共鸣与在世，只改共创：处理她的待决策前后对比。
	before := service.ComputeAttachment(ctx, rec.ID, 0.5, 5)
	for i := 0; i < 6; i++ {
		if err := service.ResolveFateDecision(ctx, "s1", rec.ID, "d"+string(rune('a'+i)), "intervene"); err != nil {
			t.Fatalf("处理待决策失败: %v", err)
		}
	}
	after := service.ComputeAttachment(ctx, rec.ID, 0.5, 5)

	if after <= before {
		t.Fatalf("共创（处理待决策）应抬升牵挂：before=%v after=%v", before, after)
	}
	// 深牵挂角色：高忠诚 + 久陪伴 + 多共创 → 明显更高。
	deep := service.ComputeAttachment(ctx, rec.ID, 0.95, 30)
	if deep <= after {
		t.Fatalf("高忠诚+久陪伴应进一步抬升：after=%v deep=%v", after, deep)
	}
}
