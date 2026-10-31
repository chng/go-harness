// Package ticket 是「工单」业务能力插件：建单属于可逆写，需要幂等但不是涉钱操作。
package ticket

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/zhichengtrang/go-harness/pkg/action"
	"github.com/zhichengtrang/go-harness/pkg/kernel"
	"github.com/zhichengtrang/go-harness/pkg/tools"
)

// Ticket 是一张售后工单。
type Ticket struct {
	ID         string    `json:"id"`
	CustomerID string    `json:"customerId"`
	OrderID    string    `json:"orderId,omitempty"`
	Subject    string    `json:"subject"`
	Detail     string    `json:"detail,omitempty"`
	Status     string    `json:"status"`
	SLA        string    `json:"sla"`
	CreatedAt  time.Time `json:"createdAt"`
}

// Service 是工单能力。
type Service struct {
	mu      sync.Mutex
	tickets []Ticket
	seq     int
}

// NewService 创建服务。
func NewService() *Service { return &Service{} }

// Plugin 把工单能力装成内核插件。
func Plugin() kernel.Plugin {
	return kernel.Plugin{Name: "capability-ticket", Provides: []string{"capability.ticket"}}
}

// CreateArgs 是 ticket.create 入参。
type CreateArgs struct {
	CustomerID string `json:"customerId" jsonschema:"客户号"`
	OrderID    string `json:"orderId,omitempty" jsonschema:"关联订单号，可选"`
	Subject    string `json:"subject" jsonschema:"工单标题（一句话）"`
	Detail     string `json:"detail,omitempty" jsonschema:"补充说明"`
}

// CreateResult 是 ticket.create 出参。
type CreateResult struct {
	TicketID string `json:"ticketId"`
	Status   string `json:"status"`
	SLA      string `json:"sla"`
}

// ListArgs 是 ticket.list 入参。
type ListArgs struct {
	CustomerID string `json:"customerId" jsonschema:"客户号"`
}

// ListResult 是 ticket.list 出参。
type ListResult struct {
	Tickets []Ticket `json:"tickets"`
}

// Register 注册动作。
func Register(svc *Service, cat *action.Catalog, reg *tools.Registry) error {
	if err := action.Register(cat, reg, action.Meta{
		Name: "ticket.create", Capability: "ticket", Risk: action.RiskWriteSoft, Idempotent: true,
		Desc: "创建售后工单。可逆写操作：不需要人工审批，但同一会话内重复调用只会建一张。",
	}, func(ctx context.Context, in CreateArgs) (CreateResult, error) {
		return svc.Create(in)
	}); err != nil {
		return err
	}
	return action.Register(cat, reg, action.Meta{
		Name: "ticket.list", Capability: "ticket", Risk: action.RiskRead, ReadOnly: true,
		Desc: "列出某客户的售后工单。",
	}, func(ctx context.Context, in ListArgs) (ListResult, error) {
		return svc.List(in)
	})
}

// Create 建单。
func (s *Service) Create(in CreateArgs) (CreateResult, error) {
	if strings.TrimSpace(in.CustomerID) == "" {
		return CreateResult{}, fmt.Errorf("customerId 不能为空")
	}
	if strings.TrimSpace(in.Subject) == "" {
		return CreateResult{}, fmt.Errorf("subject 不能为空")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	t := Ticket{
		ID: fmt.Sprintf("TK-%05d", s.seq), CustomerID: in.CustomerID, OrderID: in.OrderID,
		Subject: in.Subject, Detail: in.Detail, Status: "open", SLA: "24 小时内响应",
		CreatedAt: time.Now().UTC(),
	}
	s.tickets = append(s.tickets, t)
	return CreateResult{TicketID: t.ID, Status: t.Status, SLA: t.SLA}, nil
}

// List 列出客户的工单。
func (s *Service) List(in ListArgs) (ListResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out ListResult
	for _, t := range s.tickets {
		if t.CustomerID == in.CustomerID {
			out.Tickets = append(out.Tickets, t)
		}
	}
	sort.Slice(out.Tickets, func(i, j int) bool { return out.Tickets[i].ID < out.Tickets[j].ID })
	return out, nil
}
