package verticle

import "context"

// ConfigSource is a remote config center. The methods return config in the
// shape the file has, so Run can merge them before decoding. nacos.Client
// implements it; verticle declares the interface so it imports no config-
// center SDK. A test fake implements the same methods.
type ConfigSource interface {
	// Framework returns the framework keys of a remote framework body,
	// minus the section belonging to svcName, plus the keys the framework
	// schema does not know.
	Framework(body []byte, svcName string) (map[string]any, []string, error)
	// Section returns a remote service section. An empty body is not an
	// error: it means this DataID is not configured.
	Section(body []byte, svcName string) (map[string]any, error)
	// Watch fetches the remote config, calls onChange once with the
	// framework keys and the service section (nil when not configured),
	// and calls it again after every remote change.
	Watch(ctx context.Context, onChange func(fw, svc map[string]any)) error
}
