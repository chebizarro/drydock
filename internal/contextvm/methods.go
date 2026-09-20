package contextvm

// MethodProvider is implemented by any package that contributes ContextVM
// methods to the shared router.
type MethodProvider interface {
	RegisterContextVMMethods(router *Router) error
}

// RegisterMethods registers every provider's ContextVM methods on router.
// Nil providers are skipped so callers can pass optional handlers directly.
func RegisterMethods(router *Router, providers ...MethodProvider) error {
	for _, provider := range providers {
		if provider == nil {
			continue
		}
		if err := provider.RegisterContextVMMethods(router); err != nil {
			return err
		}
	}
	return nil
}
