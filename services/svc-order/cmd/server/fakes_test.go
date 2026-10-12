package main

import (
	"context"
	"sync"

	"github.com/qifalab/euler-platform/pricing"
)

// fakeQuoter stands in for svc-catalog. Prices are per unit (and per month
// for PREPAID), multiplied the way the real engine does.
type fakeQuoter struct {
	mu     sync.Mutex
	prices map[string]quoteResult // skuCode → unit price + default charge type
	err    error
	calls  []quoteInput
}

func (q *fakeQuoter) Quote(_ context.Context, _ int64, in quoteInput) (quoteResult, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.calls = append(q.calls, in)
	if q.err != nil {
		return quoteResult{}, q.err
	}
	unit, ok := q.prices[in.SKUCode]
	if !ok {
		return quoteResult{}, &downstreamError{Status: 404, Code: "Catalog.SKUNotFound", Message: "规格不存在或与产品不匹配"}
	}
	ct := unit.ChargeType
	if in.ChargeType != "" {
		ct = pricing.ChargeType(in.ChargeType)
	}
	payable := unit.Payable.Mul(in.Quantity)
	if ct == pricing.ChargePrepaid {
		payable = payable.Mul(in.Duration)
	}
	return quoteResult{Payable: payable, ChargeType: ct, DurationUnit: unit.DurationUnit, RegionScope: unit.RegionScope}, nil
}

// fakePayer stands in for svc-payment: a balance in 分 with idempotent debits
// and at-most-once refunds per debit key.
type fakePayer struct {
	mu       sync.Mutex
	balance  int64
	err      error
	debits   map[string]int64 // idemKey → amount
	refunded map[string]bool  // idemKey → refunded
	calls    int
	refunds  int // Refund calls
	// beforeConsume / afterConsume run (outside the lock) around a debit, to
	// interleave a concurrent request deterministically.
	beforeConsume, afterConsume func()
}

func (p *fakePayer) Consume(_ context.Context, _ int64, amountMinor int64, idemKey, _, _ string) (string, error) {
	if hook := p.beforeConsume; hook != nil {
		p.beforeConsume = nil
		hook()
	}
	id, err := p.consume(amountMinor, idemKey)
	if hook := p.afterConsume; hook != nil && err == nil {
		p.afterConsume = nil
		hook()
	}
	return id, err
}

func (p *fakePayer) consume(amountMinor int64, idemKey string) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	if p.err != nil {
		return "", p.err
	}
	if prior, done := p.debits[idemKey]; done {
		if prior != amountMinor {
			return "", errPaymentConflict
		}
		return "entry-" + idemKey, nil
	}
	if p.balance < amountMinor {
		return "", errInsufficientBalance
	}
	p.balance -= amountMinor
	p.debits[idemKey] = amountMinor
	return "entry-" + idemKey, nil
}

func (p *fakePayer) Refund(_ context.Context, _ int64, consumeKey string, amountMinor int64, _ string) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.refunds++
	if p.err != nil {
		return false, p.err
	}
	debited, ok := p.debits[consumeKey]
	if !ok {
		return false, nil
	}
	if debited != amountMinor {
		return false, errPaymentConflict
	}
	if p.refunded == nil {
		p.refunded = map[string]bool{}
	}
	if !p.refunded[consumeKey] {
		p.refunded[consumeKey] = true
		p.balance += debited
	}
	return true, nil
}

// newTestStore is the in-memory demo store (seeded order 9001: euecs.c1,
// payable ¥2160) wired to fakes instead of the real downstream services.
func newTestStore() (*orderStore, *fakeQuoter, *fakePayer) {
	s := newInMemoryOrderStore()
	q := &fakeQuoter{prices: map[string]quoteResult{
		"euecs.c1":      {Payable: pricing.MustParseAmount("21.6"), ChargeType: pricing.ChargePrepaid, DurationUnit: pricing.DurationMonth},
		"euecs.c1.post": {Payable: pricing.MustParseAmount("0.35"), ChargeType: pricing.ChargePostpaid, DurationUnit: pricing.DurationHour},
		"euoss.std":     {Payable: pricing.MustParseAmount("0.123456"), ChargeType: pricing.ChargePrepaid, DurationUnit: pricing.DurationMonth},
	}}
	p := &fakePayer{balance: 1_000_000, debits: map[string]int64{}}
	s.quoter, s.payer = q, p
	return s, q, p
}
