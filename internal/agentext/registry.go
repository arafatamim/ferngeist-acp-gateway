package agentext

import "fmt"

// chainKey identifies the ordered transformers for one message shape and stage.
type chainKey struct {
	selector Selector
	stage    Stage
}

// Registry is an immutable, validated snapshot of a Set. One Registry is built
// per session from the harness profile's registrations.
type Registry struct {
	chains map[chainKey][]Transformer
}

// New validates set and snapshots it. Duplicate names, nil callbacks, invalid
// selectors, a deferred BeforeState chain, and chains that mix execution mode or
// failure policy are rejected.
func New(set Set) (*Registry, error) {
	reg := &Registry{chains: make(map[chainKey][]Transformer, len(set.Transformers))}
	names := make(map[string]struct{}, len(set.Transformers))
	for _, transformer := range set.Transformers {
		if err := validateTransformer(transformer); err != nil {
			return nil, err
		}
		if _, duplicate := names[transformer.Name]; duplicate {
			return nil, fmt.Errorf("%w: duplicate name %q", ErrInvalidRegistration, transformer.Name)
		}
		names[transformer.Name] = struct{}{}
		key := chainKey{selector: transformer.Selector, stage: transformer.Stage}
		reg.chains[key] = append(reg.chains[key], transformer)
	}
	for key, chain := range reg.chains {
		if err := validateChain(chain); err != nil {
			return nil, fmt.Errorf("%w: %s/%s: %w", ErrInvalidRegistration, key.selector.Method, chain[0].Name, err)
		}
	}
	return reg, nil
}

// validateTransformer checks one registration on its own.
func validateTransformer(t Transformer) error {
	invalid := func(format string, args ...any) error {
		return fmt.Errorf("%w: %s: %s", ErrInvalidRegistration, t.Name, fmt.Sprintf(format, args...))
	}
	switch {
	case t.Name == "":
		return fmt.Errorf("%w: empty name", ErrInvalidRegistration)
	case t.Run == nil:
		return invalid("nil callback")
	case t.Selector.Direction > ToClient:
		return invalid("unknown direction %d", t.Selector.Direction)
	case t.Selector.Kind > Reply:
		return invalid("unknown kind %d", t.Selector.Kind)
	case t.Selector.Kind != Reply && t.Selector.Method == "":
		return invalid("empty method for a request or notification")
	case t.Stage > BeforeDelivery:
		return invalid("unknown stage %d", t.Stage)
	case t.Mode > Deferred:
		return invalid("unknown mode %d", t.Mode)
	case t.Failure > Reject:
		return invalid("unknown failure policy %d", t.Failure)
	case t.Stage == BeforeState && t.Mode != Inline:
		return invalid("BeforeState must run inline")
	case t.Mode == Deferred && (t.Stage != BeforeDelivery || !t.IndependentReply):
		return invalid("deferred work requires BeforeDelivery and IndependentReply")
	}
	return nil
}

// validateChain checks that every transformer matching one selector and stage
// shares an execution mode and a failure policy, so the chain has one outcome.
func validateChain(chain []Transformer) error {
	for _, t := range chain[1:] {
		if t.Mode != chain[0].Mode {
			return fmt.Errorf("mixed execution modes")
		}
		if t.Failure != chain[0].Failure {
			return fmt.Errorf("mixed failure policies")
		}
	}
	return nil
}
