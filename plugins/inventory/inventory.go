// Package inventory 是「库存」业务能力插件：纯只读，不需要审批也不需要幂等。
package inventory

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/zhichengtrang/go-harness/pkg/action"
	"github.com/zhichengtrang/go-harness/pkg/kernel"
	"github.com/zhichengtrang/go-harness/pkg/tools"
)

// Stock 是某个 SKU 在某仓库的库存。
type Stock struct {
	SKU       string `json:"sku"`
	Warehouse string `json:"warehouse"`
	Available int    `json:"available"`
	ETA       string `json:"eta"`
}

// Service 是库存能力。
type Service struct {
	mu    sync.RWMutex
	stock map[string]Stock
}

// NewService 创建服务并写入演示数据。
func NewService() *Service {
	rows := []Stock{
		{SKU: "SKU-1001", Warehouse: "杭州", Available: 12, ETA: "当日发货"},
		{SKU: "SKU-1001", Warehouse: "成都", Available: 0, ETA: "调货 2 天"},
		{SKU: "SKU-2002", Warehouse: "杭州", Available: 3, ETA: "次日发货"},
	}
	m := map[string]Stock{}
	for _, r := range rows {
		m[r.SKU+"/"+r.Warehouse] = r
	}
	return &Service{stock: m}
}

// Plugin 把库存能力装成内核插件。
func Plugin() kernel.Plugin {
	return kernel.Plugin{Name: "capability-inventory", Provides: []string{"capability.inventory"}}
}

// CheckArgs 是 inventory.check 入参。
type CheckArgs struct {
	SKU       string `json:"sku" jsonschema:"商品 SKU，例如 SKU-1001"`
	Warehouse string `json:"warehouse,omitempty" jsonschema:"仓库城市，不填则查全部"`
}

// CheckResult 是 inventory.check 出参。
type CheckResult struct {
	Rows []Stock `json:"rows"`
}

// Register 注册动作。
func Register(svc *Service, cat *action.Catalog, reg *tools.Registry) error {
	return action.Register(cat, reg, action.Meta{
		Name: "inventory.check", Capability: "inventory", Risk: action.RiskRead, ReadOnly: true,
		Desc: "查询商品库存与预计发货时间。只读，可并发调用。",
	}, func(ctx context.Context, in CheckArgs) (CheckResult, error) {
		return svc.Check(in)
	})
}

// Check 查库存。
func (s *Service) Check(in CheckArgs) (CheckResult, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sku := strings.ToUpper(strings.TrimSpace(in.SKU))
	var out CheckResult
	for _, st := range s.stock {
		if st.SKU != sku {
			continue
		}
		if in.Warehouse != "" && st.Warehouse != in.Warehouse {
			continue
		}
		out.Rows = append(out.Rows, st)
	}
	if len(out.Rows) == 0 {
		return out, fmt.Errorf("SKU %s 没有库存记录", in.SKU)
	}
	return out, nil
}
