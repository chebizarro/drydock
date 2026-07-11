package contextvm

// IDEGateway registers IDE gateway ContextVM handlers.
type IDEGateway interface {
	RegisterContextVMHandlers(router *Router) error
}

// RegisterIDEMethods registers IDE gateway ContextVM handlers.
func RegisterIDEMethods(router *Router, gateway IDEGateway) error {
	if gateway == nil {
		return nil
	}
	return gateway.RegisterContextVMHandlers(router)
}

// Marketplace registers marketplace ContextVM handlers.
type Marketplace interface {
	RegisterContextVMMethods(router *Router) error
}

// RegisterMarketplaceMethods registers marketplace ContextVM handlers.
func RegisterMarketplaceMethods(router *Router, marketplace Marketplace) error {
	if marketplace == nil {
		return nil
	}
	return marketplace.RegisterContextVMMethods(router)
}
