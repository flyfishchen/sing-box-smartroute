package smartroute

import (
	"context"

	box "github.com/sagernet/sing-box"
	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/adapter/outbound"
	"github.com/sagernet/sing-box/common/urltest"
	"github.com/sagernet/sing-box/include"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/service"
)

func OutboundRegistry() *outbound.Registry {
	registry := include.OutboundRegistry()
	RegisterURLTest(registry)
	return registry
}

func Context(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx = include.Context(ctx)
	if existing, ok := service.FromContext[adapter.OutboundRegistry](ctx).(*outbound.Registry); ok && existing != nil {
		RegisterURLTest(existing)
	} else {
		outboundRegistry := OutboundRegistry()
		ctx = service.ContextWith[option.OutboundOptionsRegistry](ctx, outboundRegistry)
		ctx = service.ContextWith[adapter.OutboundRegistry](ctx, outboundRegistry)
	}
	history := service.PtrFromContext[urltest.HistoryStorage](ctx)
	if history == nil {
		history = urltest.NewHistoryStorage()
		ctx = service.ContextWithPtr(ctx, history)
	}
	if service.PtrFromContext[MetricsStorage](ctx) == nil {
		ctx = service.ContextWithPtr(ctx, NewMetricsStorage(history))
	}
	return ctx
}

func NewBox(options box.Options) (*box.Box, error) {
	options.Context = Context(options.Context)
	return box.New(options)
}
