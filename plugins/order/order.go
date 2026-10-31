// Package order 是「订单」业务能力插件：查询订单 + 退款（涉钱，需审批）。
//
// 这是业务能力插件的范例：它只关心自己的领域，不碰 prompt、不碰重试、
// 不碰审批与幂等——这些由框架的中间件统一处理。
package order

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/zhichengtrang/go-harness/pkg/action"
	"github.com/zhichengtrang/go-harness/pkg/kernel"
	"github.com/zhichengtrang/go-harness/pkg/tools"
)

// Order 是订单（demo 用内存数据，真实系统换成数据库）。
type Order struct {
	ID         string    `json:"id"`
	CustomerID string    `json:"customerId"`
	Item       string    `json:"item"`
	Amount     float64   `json:"amount"`
	Refunded   float64   `json:"refunded"`
	Status     string    `json:"status"` // paid | shipped | closed
	CreatedAt  time.Time `json:"createdAt"`
}

// Refundable 返回可退金额。
func (o Order) Refundable() float64 { return o.Amount - o.Refunded }

// Service 是订单能力。
type Service struct {
	mu     sync.RWMutex
	orders map[string]*Order
	seq    int
}

// NewService 创建服务并写入演示数据。
func NewService() *Service {
	s := &Service{orders: map[string]*Order{}}
	seed := []Order{
		{ID: "A1001", CustomerID: "C-88", Item: "降噪耳机 Pro", Amount: 899, Status: "shipped"},
		{ID: "A1002", CustomerID: "C-88", Item: "机械键盘 K8", Amount: 459, Status: "paid"},
		{ID: "A1003", CustomerID: "C-99", Item: "显示器 27 寸", Amount: 1699, Status: "closed"},
	}
	now := time.Now().UTC()
	for i := range seed {
		o := seed[i]
		o.CreatedAt = now.Add(-time.Duration(24*(i+1)) * time.Hour)
		s.orders[o.ID] = &o
	}
	return s
}

// Plugin 把订单能力装成内核插件。
func Plugin(svc *Service) kernel.Plugin {
	return kernel.Plugin{
		Name:     "capability-order",
		Provides: []string{"capability.order"},
	}
}

// Register 注册动作。
func Register(svc *Service, cat *action.Catalog, reg *tools.Registry) error {
	lookup := action.Meta{
		Name: "order.lookup", Capability: "order", Risk: action.RiskRead, ReadOnly: true,
		Desc: "查询订单详情：状态、金额、已退金额、是否可退。",
	}
	if err := action.Register(cat, reg, lookup, func(ctx context.Context, in LookupArgs) (LookupResult, error) {
		return svc.Lookup(in)
	}); err != nil {
		return err
	}
	refund := action.Meta{
		Name: "order.refund", Capability: "order", Risk: action.RiskMoney,
		Scopes: []string{"order:refund"}, Idempotent: true,
		Desc: "对订单发起退款。涉钱操作，必须经人工审批；不可退金额会直接拒绝。",
		Confirm: func(ctx context.Context, raw json.RawMessage) (string, bool) {
			var in RefundArgs
			if err := json.Unmarshal(raw, &in); err != nil {
				return "退款参数无法解析", true
			}
			return fmt.Sprintf("订单 %s 退款 %.2f 元，原因：%s", in.OrderID, in.Amount, in.Reason), true
		},
	}
	refund.Validate = func(ctx context.Context, raw json.RawMessage) error {
		var in RefundArgs
		if err := json.Unmarshal(raw, &in); err != nil {
			return fmt.Errorf("退款参数无法解析: %w", err)
		}
		return svc.ValidateRefund(in)
	}
	return action.Register(cat, reg, refund, func(ctx context.Context, in RefundArgs) (RefundResult, error) {
		return svc.Refund(in)
	})
}

// LookupArgs 是 order.lookup 入参。
type LookupArgs struct {
	OrderID    string `json:"orderId,omitempty" jsonschema:"订单号，例如 A1001"`
	CustomerID string `json:"customerId,omitempty" jsonschema:"客户号，例如 C-88"`
}

// LookupResult 是 order.lookup 出参。
type LookupResult struct {
	Orders []OrderSummary `json:"orders"`
	Note   string         `json:"note,omitempty"`
}

// OrderSummary 是订单摘要。
type OrderSummary struct {
	OrderID    string  `json:"orderId"`
	Item       string  `json:"item"`
	Amount     float64 `json:"amount"`
	Refunded   float64 `json:"refunded"`
	Refundable float64 `json:"refundable"`
	Status     string  `json:"status"`
}

// Lookup 查询订单。
func (s *Service) Lookup(in LookupArgs) (LookupResult, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out LookupResult
	add := func(o *Order) {
		out.Orders = append(out.Orders, OrderSummary{
			OrderID: o.ID, Item: o.Item, Amount: o.Amount, Refunded: o.Refunded,
			Refundable: o.Refundable(), Status: o.Status,
		})
	}
	switch {
	case in.OrderID != "":
		o, ok := s.orders[strings.ToUpper(strings.TrimSpace(in.OrderID))]
		if !ok {
			return out, fmt.Errorf("订单 %s 不存在", in.OrderID)
		}
		add(o)
	case in.CustomerID != "":
		for _, o := range s.orders {
			if o.CustomerID == in.CustomerID {
				add(o)
			}
		}
		sort.Slice(out.Orders, func(i, j int) bool { return out.Orders[i].OrderID < out.Orders[j].OrderID })
		if len(out.Orders) == 0 {
			return out, fmt.Errorf("客户 %s 没有订单", in.CustomerID)
		}
	default:
		return out, fmt.Errorf("orderId 与 customerId 至少给一个")
	}
	if len(out.Orders) == 1 && out.Orders[0].Refundable <= 0 {
		out.Note = "该订单已无可退金额"
	}
	return out, nil
}

// RefundArgs 是 order.refund 入参。
type RefundArgs struct {
	OrderID string  `json:"orderId" jsonschema:"订单号"`
	Amount  float64 `json:"amount" jsonschema:"退款金额（元），不得超过可退金额"`
	Reason  string  `json:"reason" jsonschema:"退款原因，会写入审批单与审计"`
}

// RefundResult 是 order.refund 出参。
type RefundResult struct {
	RefundID string  `json:"refundId"`
	OrderID  string  `json:"orderId"`
	Amount   float64 `json:"amount"`
	Status   string  `json:"status"`
}

// ValidateRefund 是退款预检（审批前先挡掉不可能成立的单）。
func (s *Service) ValidateRefund(in RefundArgs) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	id := strings.ToUpper(strings.TrimSpace(in.OrderID))
	o, ok := s.orders[id]
	if !ok {
		return fmt.Errorf("订单 %s 不存在", in.OrderID)
	}
	if o.Status == "closed" {
		return fmt.Errorf("订单 %s 已关闭，不能退款", id)
	}
	if in.Amount <= 0 {
		return fmt.Errorf("退款金额必须大于 0")
	}
	if in.Amount > o.Refundable()+1e-9 {
		return fmt.Errorf("退款金额 %.2f 超过可退金额 %.2f", in.Amount, o.Refundable())
	}
	return nil
}

// Refund 执行退款。
func (s *Service) Refund(in RefundArgs) (RefundResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := strings.ToUpper(strings.TrimSpace(in.OrderID))
	o, ok := s.orders[id]
	if !ok {
		return RefundResult{}, fmt.Errorf("订单 %s 不存在", in.OrderID)
	}
	if o.Status == "closed" {
		return RefundResult{}, fmt.Errorf("订单 %s 已关闭，不能退款", id)
	}
	if in.Amount <= 0 {
		return RefundResult{}, fmt.Errorf("退款金额必须大于 0")
	}
	if in.Amount > o.Refundable()+1e-9 {
		return RefundResult{}, fmt.Errorf("退款金额 %.2f 超过可退金额 %.2f", in.Amount, o.Refundable())
	}
	o.Refunded += in.Amount
	s.seq++
	return RefundResult{
		RefundID: fmt.Sprintf("RF-%s-%03d", id, s.seq),
		OrderID:  id, Amount: in.Amount, Status: "refunded",
	}, nil
}
